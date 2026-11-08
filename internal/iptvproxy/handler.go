package iptvproxy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	// shares holds every currently-valid temporary share link (see
	// handleShare/handleCreateShare).
	shares *shareStore
	// trust decides whose X-Forwarded-* headers count and who may reach
	// /admin/* (see trust.go).
	trust trustPolicy
	// adminAuth is the optional /admin/* login (see adminOnly).
	adminAuth *Credentials
}

func NewProxy(cfg *Config, logger *slog.Logger, metricsEnabled bool) (*Proxy, error) {
	trust, err := newTrustPolicy(cfg.TrustedProxies, cfg.AdminAllowedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("trust config: %w", err)
	}
	p := &Proxy{logger: logger, shares: newShareStore(), trust: trust, adminAuth: cfg.AdminAuth}
	if len(cfg.AdminAllowedCIDRs) == 0 || (len(cfg.TrustedProxies) == 0 && cfg.AdminAuth == nil) {
		logger.Warn("admin UI disabled: set admin_allowed_cidrs and either trusted_proxies or admin_auth in config.yaml to enable it")
	}

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
		svc, err := newFeedService(feedCfg, repo, feedLogger, metrics, cfg.ClientBuffer)
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
	admin := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, p.withRequestLogger(p.adminOnly(h)))
	}
	mux.HandleFunc("/playlist.m3u8", p.withRequestLogger(p.handlePlaylist))
	mux.HandleFunc("/", p.withRequestLogger(p.handlePlaylist))
	mux.HandleFunc("/epg", p.withRequestLogger(p.handleEPG))
	mux.HandleFunc("/player_api.php", p.withRequestLogger(p.handleXtreamAPI))
	mux.HandleFunc("/panel_api.php", p.withRequestLogger(p.handleXtreamAPI))
	mux.HandleFunc("/share/{token}", p.withRequestLogger(p.handleShare))
	admin("/admin", p.handleAdminPage)
	admin("/admin/static/{file}", p.handleAdminStatic)
	admin("/admin/feeds", p.handleAdminFeeds)
	admin("/admin/channels", p.handleAdminChannels)
	admin("GET /admin/shares", p.handleListShares)
	admin("POST /admin/shares", p.handleCreateShare)
	admin("DELETE /admin/shares/{token}", p.handleRevokeShare)
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

// WriteString forwards to the wrapped writer so a string body isn't copied to
// []byte first; a cached playlist can be tens of MB and apps poll it often.
func (r *statusRecorder) WriteString(s string) (int, error) {
	return io.WriteString(r.ResponseWriter, s)
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
// own metrics under the given route label and the host the client reached
// this proxy at. Which feed that is isn't known until authentication
// resolves it, so — unlike the old per-feed handler — this is called per
// request rather than wrapped once at Mux() build time.
func (s *feedService) instrument(r route, host string, w http.ResponseWriter, next func(w http.ResponseWriter)) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	next(rec)
	s.metrics.observeRequest(r, host, rec.status, time.Since(start))
}

// withRequestLogger stamps every request with its own request_id and attaches
// a logger carrying it to the request context, so every log line produced
// while handling this request — in this handler, the service, and the
// repository — can be grouped together by grepping on request_id. It's also
// the one wrapper every app route shares, so it drops X-Forwarded-* headers
// from untrusted peers before any handler can read them.
func (p *Proxy) withRequestLogger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.trust.stripUntrustedForwarded(r)
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

// feedByName finds the configured feed with the given name, for admin
// endpoints that take a feed name directly rather than deriving it from
// request credentials (see matchFeed). Same linear-scan rationale applies:
// once per request, over a handful of feeds.
func (p *Proxy) feedByName(name string) *feedService {
	for _, f := range p.feeds {
		if f.name == name {
			return f
		}
	}
	return nil
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
		"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "host", requestHost(r))
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// requestCredentials reads the credentials a request presents, accepting
// either ?username=&password= query params or HTTP Basic Auth so both "M3U
// URL" and "Xtream login" style IPTV apps work.
func requestCredentials(r *http.Request) Credentials {
	creds := credentialsFromURL(r.URL)
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
	f.instrument(routePlaylist, requestHost(r), w, func(w http.ResponseWriter) {
		loggerFromContext(r.Context(), f.logger).InfoContext(r.Context(), "playlist request",
			"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "host", requestHost(r))
		f.servePlaylist(r.Context(), w, r.Header, requestBaseURL(r))
	})
}

// handleEPG serves the matched feed's own XMLTV guide, fetched from its
// upstream's /xmltv.php (see feedService.epgTarget) and cached — same
// credential resolution as handlePlaylist, so an IPTV app can point its EPG
// URL at this proxy with the same feed credentials it uses for the playlist.
func (p *Proxy) handleEPG(w http.ResponseWriter, r *http.Request) {
	f := p.authenticate(w, r)
	if f == nil {
		return
	}
	f.instrument(routeEPG, requestHost(r), w, func(w http.ResponseWriter) {
		loggerFromContext(r.Context(), f.logger).InfoContext(r.Context(), "epg request",
			"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "host", requestHost(r))
		f.serveEPG(r.Context(), w)
	})
}

// handleXtreamAPI serves the matched feed's player_api.php (or its
// panel_api.php alias) — same credential resolution as handlePlaylist/
// handleEPG, so an Xtream Codes app pointed at this proxy with this feed's
// credentials can log in and browse categories/streams/VOD/series exactly as
// it would against the real upstream, without ever seeing the upstream's own
// host or credentials.
func (p *Proxy) handleXtreamAPI(w http.ResponseWriter, r *http.Request) {
	f := p.authenticate(w, r)
	if f == nil {
		return
	}
	f.instrument(routeXtreamAPI, requestHost(r), w, func(w http.ResponseWriter) {
		loggerFromContext(r.Context(), f.logger).InfoContext(r.Context(), "xtream api request",
			"method", r.Method, "path", r.URL.Path, "action", r.URL.Query().Get("action"), "remote_addr", r.RemoteAddr, "host", requestHost(r))
		f.serveXtreamAPI(r.Context(), w, r.Header, r.URL.RawQuery, requestBaseURL(r))
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

	f.instrument(routeStream, requestHost(r), w, func(w http.ResponseWriter) {
		f.serve(r.Context(), w, r.Header, target, requestBaseURL(r))
	})
}

// handleShare serves a temporary, token-scoped share created by
// handleCreateShare. A share carries no feed credentials of its own — token
// possession plus not-yet-expired is the whole auth check — so an expired or
// unknown token gets the same 404 as one that never existed. A nested
// playlist URI arrives as an encrypted ref (see shareURIWriter) that only
// this share's key opens, so a viewer can neither read the upstream URL (and
// the credentials in it) nor forge a ref to some other path; the decrypted
// URL is still held to the share's own upstream scheme and host.
func (p *Proxy) handleShare(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sh, ok := p.shares.lookup(token)
	if !ok {
		http.NotFound(w, r)
		return
	}

	target := sh.target
	if ref := r.URL.Query().Get("ref"); ref != "" {
		u, err := sh.openRef(ref)
		if err != nil || u.Scheme != sh.target.Scheme || u.Host != sh.target.Host {
			http.Error(w, "bad ref param", http.StatusBadRequest)
			return
		}
		target = u
	}

	sh.feed.metrics.sessionStarted(sh.kind)
	defer sh.feed.metrics.sessionEnded(sh.kind)

	sh.feed.instrument(routeShare, requestHost(r), w, func(w http.ResponseWriter) {
		loggerFromContext(r.Context(), sh.feed.logger).InfoContext(r.Context(), "share request",
			"feed", sh.feed.name, "method", r.Method, "remote_addr", r.RemoteAddr, "host", requestHost(r))
		sh.feed.serveWith(r.Context(), w, r.Header, target, requestBaseURL(r), shareURIWriter(token, sh))
	})
}

// maxCreateShareBody bounds how much of an /admin/shares request body this
// proxy will buffer — the request is a few short JSON fields, never anything
// approaching the size of a playlist or segment.
const maxCreateShareBody = 4 << 10

// createShareRequest is the POST /admin/shares JSON body: which feed to
// share from, the exact channel link a client already uses against that
// feed (see parseChannelPath), and how long the resulting share stays valid.
// ChannelName is purely a display label (the admin page's combobox text for
// whatever channel_url resolves to) carried through to shareInfo so the
// active-shares list can show which channel a share is for — it's never used
// to resolve the target, so an absent or wrong value can't grant more access.
type createShareRequest struct {
	Feed        string `json:"feed"`
	ChannelURL  string `json:"channel_url"`
	ChannelName string `json:"channel_name"`
	TTL         string `json:"ttl"`
}

// handleCreateShare mints a temporary share for one channel of one feed,
// given the exact channel URL a client already uses against that feed (a
// share itself carries no credentials of its own). Every /admin/* route,
// this one included, is wrapped in adminOnly (trusted proxy + allowed client
// CIDR) and relies on Traefik's admin router for login — LAN-only, gated by
// the tinyauth forward-auth middleware (OIDC login via Pocket ID) — see
// docker-compose.yml.
// It resolves req.ChannelURL back to the real upstream URL through the same
// feed.streamTarget path a normal /stream request would, so a share can never
// point anywhere a client couldn't already reach with valid feed credentials.
func (p *Proxy) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	logger := loggerFromContext(r.Context(), p.logger)

	var req createShareRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCreateShareBody)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	feed := p.feedByName(req.Feed)
	if feed == nil {
		http.Error(w, "unknown feed", http.StatusNotFound)
		return
	}

	ttl, err := time.ParseDuration(req.TTL)
	if err != nil || ttl <= 0 {
		http.Error(w, "ttl must be a positive duration (e.g. \"4h\")", http.StatusBadRequest)
		return
	}

	channelURL, err := url.Parse(req.ChannelURL)
	if err != nil || !channelURL.IsAbs() {
		http.Error(w, "invalid channel_url", http.StatusBadRequest)
		return
	}
	prefix, username, password, rest, ok := parseChannelPath(channelURL.Path)
	if !ok {
		http.Error(w, "channel_url is not a recognized stream link", http.StatusBadRequest)
		return
	}
	userOK, passOK := feed.matches(Credentials{Username: username, Password: password})
	if !userOK || !passOK {
		http.Error(w, "channel_url credentials do not match feed", http.StatusBadRequest)
		return
	}

	target := feed.streamTarget(prefix, rest, channelURL.RawQuery)
	token, expiresAt := p.shares.create(feed, target, classifySessionKind(prefix), ttl, req.ChannelName)

	logger.InfoContext(r.Context(), "share created", "feed", feed.name, "kind", classifySessionKind(prefix), "expires_at", expiresAt)

	// An HTML fragment, not JSON: the admin page's #result swaps this
	// directly in (hx-target="#result" hx-swap="innerHTML" on the form), the
	// same server-rendered-fragment pattern every other /admin/* route uses.
	shareURL := requestBaseURL(r) + "/share/" + token
	renderAdminFragment(w, "share-created", struct{ Channel, Expires, URL string }{
		Channel: cmp.Or(req.ChannelName, "this channel"),
		Expires: expiresAt.Format("2006-01-02 15:04:05 MST"),
		URL:     shareURL,
	})
}

// handleRevokeShare drops one share immediately instead of waiting out its
// TTL (see shareStore.revoke) — same adminOnly gating as handleCreateShare,
// since revoking is exactly as privileged an action as minting.
func (p *Proxy) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	logger := loggerFromContext(r.Context(), p.logger)

	token := r.PathValue("token")
	if !p.shares.revoke(token) {
		http.NotFound(w, r)
		return
	}
	logger.InfoContext(r.Context(), "share revoked")
	// 200 with an empty body, not 204: htmx doesn't swap on a 204, which would
	// leave the revoked row on the admin page.
	w.WriteHeader(http.StatusOK)
}
