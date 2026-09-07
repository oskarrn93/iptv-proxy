package iptvproxy

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// feed is one configured feed's runtime pieces: the credentials a client
// authenticates with, and the service that knows how to serve it.
type feed struct {
	name     string
	username string
	password string
	svc      *feedService
	metrics  *feedMetrics
}

// Proxy is the single HTTP entry point for every configured feed. All feeds
// share one listen address; a request's username/password (query params or
// HTTP Basic Auth) is matched against every feed's own credentials to pick
// which one's upstream serves it, so exposing several upstream IPTV accounts
// no longer means exposing one port each.
type Proxy struct {
	feeds          []*feed
	logger         *slog.Logger
	metricsHandler http.Handler
}

func NewProxy(cfg *Config, logger *slog.Logger, metricsEnabled bool) (*Proxy, error) {
	p := &Proxy{logger: logger}

	// Gatherers merges every feed's own registry into one /metrics response
	// without the feeds sharing a registry outright — each newFeedMetrics
	// call still owns registration failures for just its own feed.
	var gatherers prometheus.Gatherers
	for _, feedCfg := range cfg.Feeds {
		feedLogger := logger.With("feed", feedCfg.Name)
		metrics := newFeedMetrics(feedCfg.Name, metricsEnabled, feedLogger)
		if metrics.registry != nil {
			gatherers = append(gatherers, metrics.registry)
		}
		repo := newUpstreamRepository(feedLogger, feedCfg.Timeout)
		svc, err := newFeedService(feedCfg, repo, feedLogger, metrics)
		if err != nil {
			return nil, fmt.Errorf("feed %q: %w", feedCfg.Name, err)
		}
		p.feeds = append(p.feeds, &feed{
			name:     feedCfg.Name,
			username: feedCfg.Username,
			password: feedCfg.Password,
			svc:      svc,
			metrics:  metrics,
		})
	}

	if len(gatherers) == 0 {
		p.metricsHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics disabled", http.StatusNotFound)
		})
	} else {
		p.metricsHandler = promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})
	}

	return p, nil
}

func (p *Proxy) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", p.withRequestLogger(p.handlePlaylist))
	mux.HandleFunc("/", p.withRequestLogger(p.handlePlaylist))
	mux.HandleFunc("/stream", p.withRequestLogger(p.handleStream))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", p.metricsHandler)
	return mux
}

// statusRecorder captures the status code written by a handler so instrument
// can label its metrics with it; a plain http.ResponseWriter doesn't expose
// what was already written.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// instrument runs next, then records the outcome against the matched feed's
// own metrics under the given route label. Which feed that is isn't known
// until authentication resolves it, so — unlike the old per-feed handler —
// this is called per request rather than wrapped once at Mux() build time.
func (f *feed) instrument(r route, w http.ResponseWriter, req *http.Request, next func(w http.ResponseWriter, req *http.Request)) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	next(rec, req)
	f.metrics.observeRequest(r, rec.status, time.Since(start))
}

// withRequestLogger stamps every request with its own request_id and attaches
// a logger carrying it to the request context, so every log line produced
// while handling this request — in this handler, the service, and the
// repository — can be grouped together by grepping on request_id.
func (p *Proxy) withRequestLogger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqLogger := p.logger.With("request_id", newRequestID())
		next(w, r.WithContext(withLogger(r.Context(), reqLogger)))
	}
}

// authenticate accepts either ?username=&password= query params or HTTP
// Basic Auth (so both "M3U URL" and "Xtream login" style IPTV apps work),
// and matches them against every configured feed to pick which one serves
// this request. A username that matches a known feed but the wrong password
// counts an auth failure against that feed's own metrics; a username
// matching no feed at all is logged but not attributed to any feed.
func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request) *feed {
	user := r.URL.Query().Get("username")
	pass := r.URL.Query().Get("password")
	if user == "" && pass == "" {
		if basicUser, basicPass, ok := r.BasicAuth(); ok {
			user, pass = basicUser, basicPass
		}
	}

	var candidate *feed
	for _, f := range p.feeds {
		if subtle.ConstantTimeCompare([]byte(user), []byte(f.username)) != 1 {
			continue
		}
		candidate = f
		if subtle.ConstantTimeCompare([]byte(pass), []byte(f.password)) == 1 {
			return f
		}
	}

	loggerFromContext(r.Context(), p.logger).WarnContext(r.Context(), "auth failed",
		"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
	if candidate != nil {
		candidate.metrics.authFailures.Inc()
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return nil
}

// handlePlaylist serves the matched feed's own top-level playlist (its
// configured upstream_url). Clients point their IPTV app at this proxy
// instead of the real upstream, with credentials picking which feed.
func (p *Proxy) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	f := p.authenticate(w, r)
	if f == nil {
		return
	}
	f.instrument(routePlaylist, w, r, func(w http.ResponseWriter, r *http.Request) {
		loggerFromContext(r.Context(), f.svc.logger).InfoContext(r.Context(), "playlist request",
			"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		f.svc.servePlaylist(r.Context(), w, r.Header)
	})
}

// handleStream serves rewritten segment/sub-playlist links produced by
// rewritePlaylist, fetching whatever upstream URL is given in "u" through the
// matched feed.
func (p *Proxy) handleStream(w http.ResponseWriter, r *http.Request) {
	f := p.authenticate(w, r)
	if f == nil {
		return
	}
	f.instrument(routeStream, w, r, func(w http.ResponseWriter, r *http.Request) {
		streamReq, err := f.svc.parseStreamRequest(r.Context(), r.URL.Query().Get("u"))
		if err != nil {
			var reqErr *StreamRequestError
			if errors.As(err, &reqErr) && reqErr.Forbidden {
				loggerFromContext(r.Context(), f.svc.logger).WarnContext(r.Context(), "rejected stream target",
					"remote_addr", r.RemoteAddr, "error", err)
				http.Error(w, "forbidden target", http.StatusForbidden)
				return
			}
			http.Error(w, "bad u param", http.StatusBadRequest)
			return
		}

		f.svc.serve(r.Context(), w, r.Header, streamReq.Target)
	})
}
