package iptvproxy

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
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
}

func NewFeedProxy(cfg FeedConfig, logger *slog.Logger) (*FeedHandler, error) {
	logger = logger.With("feed", cfg.Name)
	repo := newUpstreamRepository(logger)
	svc, err := newFeedService(cfg, repo, logger)
	if err != nil {
		return nil, err
	}
	return &FeedHandler{
		username: cfg.Username,
		password: cfg.Password,
		svc:      svc,
		logger:   logger,
	}, nil
}

func (h *FeedHandler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", h.withRequestLogger(h.handlePlaylist))
	mux.HandleFunc("/", h.withRequestLogger(h.handlePlaylist))
	mux.HandleFunc("/stream", h.withRequestLogger(h.handleStream))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
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
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
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
	raw := r.URL.Query().Get("u")
	if raw == "" {
		http.Error(w, "missing u", http.StatusBadRequest)
		return
	}
	target, err := h.svc.resolveStreamTarget(raw)
	if err != nil {
		http.Error(w, "bad u param", http.StatusBadRequest)
		return
	}
	if err := h.svc.validateStreamTarget(r.Context(), target); err != nil {
		loggerFromContext(r.Context(), h.logger).WarnContext(r.Context(), "rejected stream target",
			"remote_addr", r.RemoteAddr, "error", err)
		http.Error(w, "forbidden target", http.StatusForbidden)
		return
	}

	h.svc.serve(r.Context(), w, r.Header, target)
}
