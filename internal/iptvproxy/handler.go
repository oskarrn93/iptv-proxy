package iptvproxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Proxy is the single HTTP entry point for every configured feed. All feeds
// share one listen address; a request's username/password (query params or
// HTTP Basic Auth) is matched against every feed's own credentials to pick
// which one's upstream serves it, so exposing several upstream IPTV accounts
// no longer means exposing one port each.
type Proxy struct {
	feeds          []*feedService
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
		p.feeds = append(p.feeds, svc)
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
	mux.HandleFunc("/{prefix}/{username}/{password}/{rest...}", p.withRequestLogger(p.handleStream))
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

// Unwrap exposes the wrapped writer to http.ResponseController. Without it a
// controller can reach neither this recorder's Flush (embedding the
// http.ResponseWriter *interface* promotes only Header/Write/WriteHeader) nor
// the real writer's, so bufferedCopy's flush would silently no-op and a live
// TS relay would deliver nothing until a buffer filled.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// instrument runs next, then records the outcome against the matched feed's
// own metrics under the given route label. Which feed that is isn't known
// until authentication resolves it, so — unlike the old per-feed handler —
// this is called per request rather than wrapped once at Mux() build time.
func (s *feedService) instrument(r route, w http.ResponseWriter, next func(w http.ResponseWriter)) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	next(rec)
	s.metrics.observeRequest(r, rec.status, time.Since(start))
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

// matchFeed finds the feed whose own credentials are exactly creds. candidate
// is the feed whose username matched but whose password did not, so a failure
// can still be attributed to the right feed's metrics. A linear scan is fine:
// it runs once per request (not per playlist line), over a handful of feeds.
func (p *Proxy) matchFeed(creds Credentials) (matched, candidate *feedService) {
	for _, f := range p.feeds {
		userOK, passOK := f.matches(creds)
		if !userOK {
			continue
		}
		candidate = f
		if passOK {
			return f, nil
		}
	}
	return nil, candidate
}

// rejectUnauthorized is the single response to a request whose credentials
// match no feed: one log line, one metric, one 401 — so the playlist and
// stream routes can't drift into reporting a rejection differently.
func (p *Proxy) rejectUnauthorized(w http.ResponseWriter, r *http.Request, candidate *feedService) {
	logger := loggerFromContext(r.Context(), p.logger)
	if candidate != nil {
		candidate.metrics.authFailures.Inc()
		logger = logger.With("feed", candidate.name)
	}
	logger.WarnContext(r.Context(), "auth failed",
		"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// requestCredentials reads the credentials a request presents, accepting
// either ?username=&password= query params or HTTP Basic Auth so both "M3U
// URL" and "Xtream login" style IPTV apps work.
func requestCredentials(r *http.Request) Credentials {
	query := r.URL.Query()
	creds := Credentials{Username: query.Get("username"), Password: query.Get("password")}
	if creds.Username == "" && creds.Password == "" {
		if user, pass, ok := r.BasicAuth(); ok {
			creds = Credentials{Username: user, Password: pass}
		}
	}
	return creds
}

// authenticate matches a request's credentials against every configured feed
// to pick which one serves it. A username that matches a known feed but the
// wrong password counts an auth failure against that feed's own metrics; a
// username matching no feed at all is logged but not attributed to any feed.
func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request) *feedService {
	f, candidate := p.matchFeed(requestCredentials(r))
	if f == nil {
		p.rejectUnauthorized(w, r, candidate)
		return nil
	}
	return f
}

// handlePlaylist serves the matched feed's own top-level playlist (its
// configured upstream_url). Clients point their IPTV app at this proxy
// instead of the real upstream, with credentials picking which feed.
func (p *Proxy) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	f := p.authenticate(w, r)
	if f == nil {
		return
	}
	f.instrument(routePlaylist, w, func(w http.ResponseWriter) {
		loggerFromContext(r.Context(), f.logger).InfoContext(r.Context(), "playlist request",
			"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		f.servePlaylist(r.Context(), w, r.Header)
	})
}

// handleStream serves rewritten segment/sub-playlist links produced by
// rewritePlaylist. Its username/password are the matched feed's own
// credentials, written into the link where the upstream's own credentials
// stood (see writeStreamURI) — so they both say which feed the link belongs to
// and authenticate it, exactly as they do on the playlist route. They arrive
// as path segments, /<prefix>/<username>/<password>/<file>, mimicking a
// standard Xtream stream URL; the feed's configured upstream is what turns the
// rest back into a URL to fetch (see streamTarget).
func (p *Proxy) handleStream(w http.ResponseWriter, r *http.Request) {
	f, candidate := p.matchFeed(Credentials{
		Username: r.PathValue("username"),
		Password: r.PathValue("password"),
	})
	if f == nil {
		p.rejectUnauthorized(w, r, candidate)
		return
	}

	prefix := r.PathValue("prefix")
	target := f.streamTarget(prefix, r.PathValue("rest"), r.URL.RawQuery)

	kind := classifySessionKind(prefix)
	f.metrics.sessionStarted(kind)
	defer f.metrics.sessionEnded(kind)

	f.instrument(routeStream, w, func(w http.ResponseWriter) {
		f.serve(r.Context(), w, r.Header, target)
	})
}
