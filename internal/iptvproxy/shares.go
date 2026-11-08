package iptvproxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
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
	// channelName is the display label the admin page's combobox showed for
	// this channel at creation time (from the feed's own #EXTINF entry) —
	// carried through purely so handleListShares can show which channel a
	// share is for; it plays no role in serving the share.
	channelName string
	// refs seals and opens the nested-URI refs this share's rewritten
	// playlists carry (see shareURIWriter/openRef). The key is per share and
	// never leaves the process, so a ref is opaque to the viewer, can't be
	// forged, and is useless against any other share.
	refs cipher.AEAD

	expiresAt time.Time
}

// errBadShareRef covers every way a ref can fail to open — malformed,
// truncated, tampered with, or sealed by another share — without saying which.
var errBadShareRef = errors.New("invalid share ref")

// sealRef encrypts an upstream URL into a URL-safe ref.
func (sh share) sealRef(u *url.URL) string {
	nonce := make([]byte, sh.refs.NonceSize())
	rand.Read(nonce)
	sealed := sh.refs.Seal(nonce, nonce, []byte(u.String()), nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

// openRef reverses sealRef.
func (sh share) openRef(ref string) (*url.URL, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(ref)
	if err != nil || len(sealed) < sh.refs.NonceSize() {
		return nil, errBadShareRef
	}
	nonce, ciphertext := sealed[:sh.refs.NonceSize()], sealed[sh.refs.NonceSize():]
	plain, err := sh.refs.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errBadShareRef
	}
	u, err := url.Parse(string(plain))
	if err != nil || !u.IsAbs() {
		return nil, errBadShareRef
	}
	return u, nil
}

// newShareAEAD returns AES-256-GCM under a fresh random key.
func newShareAEAD() cipher.AEAD {
	key := make([]byte, 32)
	rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("iptvproxy: aes.NewCipher: " + err.Error())
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("iptvproxy: cipher.NewGCM: " + err.Error())
	}
	return aead
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
func (s *shareStore) create(feed *feedService, target *url.URL, kind sessionKind, ttl time.Duration, channelName string) (token string, expiresAt time.Time) {
	if ttl > maxShareTTL {
		ttl = maxShareTTL
	}
	token = randShareToken()
	now := time.Now()
	expiresAt = now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked(now)
	s.shares[token] = share{feed: feed, target: target, kind: kind, channelName: channelName, refs: newShareAEAD(), expiresAt: expiresAt}
	return token, expiresAt
}

// lookup returns the share for token, or ok=false if the token is unknown or
// has expired — a request presenting an expired token gets exactly the same
// response as one presenting a token that never existed, so a share's
// lifetime can't be probed from the outside.
func (s *shareStore) lookup(token string) (sh share, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked(time.Now())
	sh, ok = s.shares[token]
	return sh, ok
}

// shareInfo is a share's own state, safe to hand back to the admin page — it
// carries no upstream credentials, just enough to identify and revoke it.
type shareInfo struct {
	Token       string
	Feed        string
	ChannelName string
	Kind        sessionKind
	ExpiresAt   time.Time
}

// list returns every currently-valid share, for the admin page's "active
// shares" view. Order is unspecified — map iteration order — since shares
// are few enough that the admin page can sort them however it likes.
func (s *shareStore) list() []shareInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.reapLocked(now)

	infos := make([]shareInfo, 0, len(s.shares))
	for token, sh := range s.shares {
		infos = append(infos, shareInfo{
			Token:       token,
			Feed:        sh.feed.name,
			ChannelName: sh.channelName,
			Kind:        sh.kind,
			ExpiresAt:   sh.expiresAt,
		})
	}
	return infos
}

// revoke drops token immediately, regardless of its remaining TTL, reporting
// whether it was actually found. A revoked share's live relay (if any) isn't
// forcibly disconnected — the viewer's current response keeps streaming to
// completion — but no new request against the token will be served past this
// point, and liveTargetTracker's own release (deferred in serveWith) frees
// the channel for a future stream as soon as that connection ends on its own.
func (s *shareStore) revoke(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.shares[token]; !ok {
		return false
	}
	delete(s.shares, token)
	return true
}

// randShareToken returns an unguessable 128-bit token — long enough that
// guessing one isn't a viable attack against a link that grants access with
// no further authentication.
func randShareToken() string {
	return randHex(16)
}

// shareURIWriter builds the uriWriter a share's own playlist rewrite pass
// uses in place of writeStreamURI: every nested segment/sub-playlist/key URI
// becomes a /share/<token>?ref=<sealed> link instead of an Xtream-shaped
// /stream one, since a share has no feed credentials of its own to write
// into the link. The resolved upstream URL is sealed rather than written
// out, because for Xtream upstreams it embeds the real upstream username and
// password in its path — a plain URL here would hand those to anyone holding
// the share link, for longer than the share lives.
func shareURIWriter(token string, sh share) uriWriter {
	return func(out *strings.Builder, resolved *url.URL) {
		out.WriteString(baseURLPlaceholder)
		out.WriteString("/share/")
		out.WriteString(token)
		out.WriteString("?ref=")
		out.WriteString(sh.sealRef(resolved))
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
