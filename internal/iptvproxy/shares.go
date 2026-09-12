package iptvproxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxShareTTL caps how long a share can stay valid, regardless of what the
// admin request asks for — a temporary link is meant to be sent to a friend
// for one viewing session, not to become a standing, unrevocable credential.
const maxShareTTL = 24 * time.Hour

// share is one token-scoped, TTL-limited link to a single upstream stream
// (see feedService.streamTarget), created via Proxy.handleCreateShare and
// served via Proxy.handleShare. Unlike a feed's own credentials, a share
// grants access to exactly one target, not that feed's whole catalog.
type share struct {
	feed   *feedService
	target *url.URL
	// kind is classified once at creation (see classifySessionKind) so
	// handleShare can bracket activeSessions the same way handleStream does,
	// without re-deriving it from a token that carries no path prefix itself.
	kind sessionKind

	expiresAt time.Time
}

// shareStore holds every currently-valid share in memory, keyed by its
// unguessable token. Shares are intentionally process-local, not persisted:
// a restart invalidating outstanding share links is an acceptable trade for
// not needing a second storage backend for what's meant to be a short-lived,
// single-viewing-session link in the first place.
type shareStore struct {
	mu     sync.Mutex
	shares map[string]share
}

func newShareStore() *shareStore {
	return &shareStore{shares: make(map[string]share)}
}

// reapLocked drops every expired entry. Called opportunistically from create
// and lookup rather than off a background ticker — mirroring
// singleflightCache's lazy, access-triggered expiry rather than adding a
// second goroutine lifecycle to this package.
func (s *shareStore) reapLocked(now time.Time) {
	for token, sh := range s.shares {
		if now.After(sh.expiresAt) {
			delete(s.shares, token)
		}
	}
}

// create mints a new share for target on feed, valid for ttl (capped at
// maxShareTTL), and returns its token and absolute expiry.
func (s *shareStore) create(feed *feedService, target *url.URL, kind sessionKind, ttl time.Duration) (token string, expiresAt time.Time) {
	if ttl > maxShareTTL {
		ttl = maxShareTTL
	}
	token = randShareToken()
	expiresAt = time.Now().Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked(time.Now())
	s.shares[token] = share{feed: feed, target: target, kind: kind, expiresAt: expiresAt}
	return token, expiresAt
}

// lookup returns the share for token, or ok=false if the token is unknown or
// has expired — a request presenting an expired token gets exactly the same
// response as one presenting a token that never existed, so a share's
// lifetime can't be probed from the outside.
func (s *shareStore) lookup(token string) (sh share, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok = s.shares[token]
	if !ok {
		return share{}, false
	}
	now := time.Now()
	if now.After(sh.expiresAt) {
		delete(s.shares, token)
		return share{}, false
	}
	s.reapLocked(now)
	return sh, true
}

// randShareToken returns an unguessable token, sized the same way as the
// standalone stream-proxy's own tokens (stream/main.go) — long enough that
// guessing one isn't a viable attack against a link that grants access with
// no further authentication.
func randShareToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the system RNG is broken, not that this
		// one token can't be generated — panicking here would only be traded
		// for panicking on the next TLS handshake instead.
		panic("iptvproxy: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// shareURIWriter builds the uriWriter a share's own playlist rewrite pass
// uses in place of writeStreamURI: every nested segment/sub-playlist/key URI
// becomes a /share/<token>?u=<resolved> link instead of an Xtream-shaped
// /stream one, since a share has no feed credentials of its own to write
// into the link. handleShare constrains an incoming u to the same upstream
// host the share was created against (see streamTarget's identical
// reasoning) before ever fetching it.
func shareURIWriter(token string) uriWriter {
	return func(out *strings.Builder, resolved *url.URL) {
		out.WriteString(baseURLPlaceholder)
		out.WriteString("/share/")
		out.WriteString(token)
		out.WriteString("?u=")
		out.WriteString(url.QueryEscape(resolved.String()))
	}
}

// parseChannelPath splits a stream link's path the same way streamURIPathParts
// composes one — /<prefix>/<username>/<password>/<rest...> — so an admin can
// hand handleCreateShare the exact channel URL a client already uses (e.g.
// copied from this feed's own rewritten playlist) instead of having to know
// this proxy's internal prefix/rest vocabulary.
func parseChannelPath(path string) (prefix, username, password, rest string, ok bool) {
	trimmed := strings.Trim(path, "/")
	segments := strings.SplitN(trimmed, "/", 4)
	if len(segments) < 3 {
		return "", "", "", "", false
	}
	if len(segments) == 4 {
		rest = segments[3]
	}
	return segments[0], segments[1], segments[2], rest, true
}
