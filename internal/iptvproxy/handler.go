package iptvproxy

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// FeedHandler is one feed's HTTP entry point: it authenticates requests,
// parses and validates request parameters, and translates the service
// layer's results into HTTP responses. It holds no business logic of its
// own beyond that request/response translation.
type FeedHandler struct {
	username string
	password string
	svc      *feedService
	logger   *slog.Logger
	metrics  *feedMetrics
}

func NewFeedProxy(cfg FeedConfig, logger *slog.Logger, metricsEnabled bool) (*FeedHandler, error) {
	logger = logger.With("feed", cfg.Name)
	metrics := newFeedMetrics(cfg.Name, metricsEnabled, logger)
	repo := newUpstreamRepository(logger)
	svc, err := newFeedService(cfg, repo, logger, metrics)
	if err != nil {
		return nil, err
	}
	return &FeedHandler{
		username: cfg.Username,
		password: cfg.Password,
		svc:      svc,
		logger:   logger,
		metrics:  metrics,
	}, nil
}

func (h *FeedHandler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", h.instrument(routePlaylist, h.withRequestLogger(h.handlePlaylist)))
	mux.HandleFunc("/", h.instrument(routePlaylist, h.withRequestLogger(h.handlePlaylist)))
	mux.HandleFunc("/stream", h.instrument(routeStream, h.withRequestLogger(h.handleStream)))
	mux.HandleFunc("/health", h.instrument(routeHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		writeHealthStatus(w, h.health())
	}))
	mux.Handle("/metrics", h.metrics.handler())
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

// instrument wraps next with request count and duration metrics under the
// given route label. route is a fixed label passed by the caller rather than
// derived from the request path, keeping cardinality bounded to the handful
// of routes this feed actually serves.
func (h *FeedHandler) instrument(r route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next(rec, req)
		h.metrics.observeRequest(r, rec.status, time.Since(start))
	}
}

// withRequestLogger stamps every request with its own request_id and attaches
// a logger carrying it to the request context, so every log line produced
// while handling this request — in this handler, the service, and the
// repository — can be grouped together by grepping on request_id.
func (h *FeedHandler) withRequestLogger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqLogger := h.logger.With("request_id", newRequestID())
		next(w, r.WithContext(withLogger(r.Context(), reqLogger)))
	}
}

// requireAuth accepts either ?username=&password= query params or HTTP Basic
// Auth so both "M3U URL" and "Xtream login" style IPTV apps work.
func (h *FeedHandler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	user := r.URL.Query().Get("username")
	pass := r.URL.Query().Get("password")
	if user == "" && pass == "" {
		if basicUser, basicPass, ok := r.BasicAuth(); ok {
			user, pass = basicUser, basicPass
		}
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) == 1
	if userOK && passOK {
		return true
	}
	loggerFromContext(r.Context(), h.logger).WarnContext(r.Context(), "auth failed",
		"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
	h.metrics.authFailures.Inc()
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// health reports this feed's own liveness, independent of how it gets
// rendered to an HTTP response.
func (h *FeedHandler) health() HealthStatus {
	return HealthStatus{Healthy: true}
}

// handlePlaylist serves the feed's own top-level playlist (the configured
// upstream_url). Clients point their IPTV app at this URL instead of the
// real upstream.
func (h *FeedHandler) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	loggerFromContext(r.Context(), h.logger).InfoContext(r.Context(), "playlist request",
		"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)

	h.svc.serve(r.Context(), w, r.Header, h.svc.playlistTarget())
}

// handleStream serves rewritten segment/sub-playlist links produced by
// rewritePlaylist, fetching whatever upstream URL is given in "u".
func (h *FeedHandler) handleStream(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	streamReq, err := h.svc.parseStreamRequest(r.Context(), r.URL.Query().Get("u"))
	if err != nil {
		var reqErr *StreamRequestError
		if errors.As(err, &reqErr) && reqErr.Forbidden {
			loggerFromContext(r.Context(), h.logger).WarnContext(r.Context(), "rejected stream target",
				"remote_addr", r.RemoteAddr, "error", err)
			http.Error(w, "forbidden target", http.StatusForbidden)
			return
		}
		http.Error(w, "bad u param", http.StatusBadRequest)
		return
	}

	h.svc.serve(r.Context(), w, r.Header, streamReq.Target)
}
