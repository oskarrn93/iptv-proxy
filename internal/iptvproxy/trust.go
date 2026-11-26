package iptvproxy

import (
	"net/http"
	"net/netip"
	"strings"
)

// forwardedHeaders are the reverse-proxy headers requestBaseURL/requestHost
// read. They're only meaningful when a trusted proxy set them — from anyone
// else they're just client-chosen strings (see stripUntrustedForwarded).
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// trustPolicy decides which peers' X-Forwarded-* headers are believed and who
// may reach /admin/*. Its zero value trusts no proxy and admits no admin
// client, so a config.yaml that omits both lists fails closed.
type trustPolicy struct {
	trustedProxies []netip.Prefix
	adminAllowed   []netip.Prefix
}

func newTrustPolicy(trustedProxies, adminAllowed []string) (trustPolicy, error) {
	tp, err := parsePrefixes(trustedProxies)
	if err != nil {
		return trustPolicy{}, err
	}
	aa, err := parsePrefixes(adminAllowed)
	if err != nil {
		return trustPolicy{}, err
	}
	return trustPolicy{trustedProxies: tp, adminAllowed: aa}, nil
}

func parsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, p.Masked())
	}
	return prefixes, nil
}

func containsAddr(prefixes []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// peerAddr is the address of whoever opened the TCP connection — the one
// value in a request a client can't simply write into a header.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

func (t trustPolicy) peerTrusted(r *http.Request) bool {
	peer, ok := peerAddr(r)
	return ok && containsAddr(t.trustedProxies, peer)
}

// clientAddr is the address of the original client: the peer itself, or —
// when the peer is a trusted proxy — the rightmost X-Forwarded-For entry that
// isn't also a trusted proxy. Rightmost, not leftmost, because every proxy
// appends; entries left of the last trusted hop were written by the client.
func (t trustPolicy) clientAddr(r *http.Request) (netip.Addr, bool) {
	peer, ok := peerAddr(r)
	if !ok {
		return netip.Addr{}, false
	}
	if !containsAddr(t.trustedProxies, peer) {
		return peer, true
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		addr, err := netip.ParseAddr(hop)
		if err != nil {
			// An unparseable hop can't be attributed to anyone; stop rather
			// than skip past it to something further left the client wrote.
			return netip.Addr{}, false
		}
		addr = addr.Unmap()
		if !containsAddr(t.trustedProxies, addr) {
			return addr, true
		}
	}
	return peer, true
}

// stripUntrustedForwarded drops X-Forwarded-* headers from requests that
// didn't come through a trusted proxy, so a client hitting the listen port
// directly can't steer the host rewritten playlist links point at.
func (t trustPolicy) stripUntrustedForwarded(r *http.Request) {
	if t.peerTrusted(r) {
		return
	}
	for _, h := range forwardedHeaders {
		r.Header.Del(h)
	}
}

// adminCrossOrigin rejects cross-site POST/DELETE requests. A browser attaches
// Basic credentials and login cookies by itself, so without this any website
// an admin visits could mint or revoke shares.
var adminCrossOrigin = http.NewCrossOriginProtection()

// hasOwnAdminLogin reports whether this proxy authenticates admin users
// itself, so it needn't rely on a trusted reverse proxy having done it.
func (p *Proxy) hasOwnAdminLogin() bool {
	return p.adminAuth != nil || p.oidc != nil
}

// adminOnly gates every /admin/* route. The original client must be inside
// admin_allowed_cidrs, checked first so nobody outside it can probe the login.
//
// With admin_auth set, the request must also carry that login. Without it,
// login is the reverse proxy's job (e.g. forward-auth middleware on its
// router), and this is the check that still holds if that routing is ever
// wrong again: the request must have arrived through a trusted proxy, so it
// passed whatever middleware that proxy applies rather than hitting the listen
// port directly.
func (p *Proxy) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context(), p.logger)
		client, ok := p.trust.clientAddr(r)
		if !ok || !containsAddr(p.trust.adminAllowed, client) || (!p.hasOwnAdminLogin() && !p.trust.peerTrusted(r)) {
			logger.WarnContext(r.Context(), "admin request denied",
				"path", r.URL.Path, "remote_addr", r.RemoteAddr, "client", client.String())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if p.oidc != nil && !p.oidc.authorize(w, r) {
			return
		}
		if p.adminAuth != nil {
			user, pass, presented := r.BasicAuth()
			userOK, passOK := p.adminAuth.matches(Credentials{Username: user, Password: pass})
			if !presented || !userOK || !passOK {
				// A browser's first request never carries credentials, so only
				// a failed attempt is worth a log line.
				if presented {
					logger.WarnContext(r.Context(), "admin login failed", "client", client.String())
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="iptv-proxy admin", charset="UTF-8"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if err := adminCrossOrigin.Check(r); err != nil {
			logger.WarnContext(r.Context(), "admin cross-origin request denied", "path", r.URL.Path, "client", client.String())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
