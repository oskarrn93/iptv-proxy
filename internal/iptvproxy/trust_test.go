package iptvproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTrustedTestProxy is newTestProxy with Traefik-like trust config: proxies
// on 172.30.0.0/16, admin allowed from the 192.168.1.0/24 LAN.
func newTrustedTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	trust, err := newTrustPolicy([]string{"172.30.0.0/16"}, []string{"192.168.1.0/24"})
	require.NoError(t, err)
	p.trust = trust
	return p
}

func adminRequest(remoteAddr, xff string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/admin/shares", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	return req
}

// TestAdminOnly covers the property that exposed /admin to the internet once
// already: login lives on a Traefik router, so the app itself must refuse any
// admin request that didn't come through the proxy from an allowed client.
func TestAdminOnly(t *testing.T) {
	p := newTrustedTestProxy(t)
	mux := p.Mux()

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       int
	}{
		{"LAN client via proxy", "172.30.0.9:40000", "192.168.1.50", http.StatusOK},
		{"public client via proxy", "172.30.0.9:40000", "203.0.113.7", http.StatusForbidden},
		{"public client spoofing a LAN hop via proxy", "172.30.0.9:40000", "192.168.1.50, 203.0.113.7", http.StatusForbidden},
		{"LAN client hitting the port directly", "192.168.1.50:40000", "", http.StatusForbidden},
		{"direct client forging X-Forwarded-For", "203.0.113.7:40000", "192.168.1.50", http.StatusForbidden},
		{"proxy with no forwarded client", "172.30.0.9:40000", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, adminRequest(tc.remoteAddr, tc.xff))
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestAdminOnly_DisabledWithoutConfig(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, adminRequest("127.0.0.1:40000", ""))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestStripUntrustedForwarded(t *testing.T) {
	p := newTrustedTestProxy(t)

	t.Run("untrusted peer loses forwarded headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:58033/playlist.m3u8", nil)
		req.RemoteAddr = "192.168.1.50:40000"
		req.Header.Set("X-Forwarded-Host", "evil.example.com")
		req.Header.Set("X-Forwarded-Proto", "https")
		p.trust.stripUntrustedForwarded(req)
		assert.Equal(t, "http://192.168.1.50:58033", requestBaseURL(req))
	})

	t.Run("trusted proxy keeps them", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://iptv-proxy:58033/playlist.m3u8", nil)
		req.RemoteAddr = "172.30.0.9:40000"
		req.Header.Set("X-Forwarded-Host", "iptv.example.com")
		req.Header.Set("X-Forwarded-Proto", "https")
		p.trust.stripUntrustedForwarded(req)
		assert.Equal(t, "https://iptv.example.com", requestBaseURL(req))
	})
}

func TestNewTrustPolicy_RejectsBadCIDR(t *testing.T) {
	_, err := newTrustPolicy([]string{"not-a-cidr"}, nil)
	assert.Error(t, err)
}

func TestAdminOnly_WithBasicAuth(t *testing.T) {
	p := newTrustedTestProxy(t)
	p.adminAuth = &Credentials{Username: "admin", Password: "hunter2"}
	mux := p.Mux()
	const (
		lanDirect = "192.168.1.50:40000"
		proxy     = "172.30.0.9:40000"
	)

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		user, pass string
		want       int
	}{
		{"LAN client direct with login", lanDirect, "", "admin", "hunter2", http.StatusOK},
		{"LAN client direct without login", lanDirect, "", "", "", http.StatusUnauthorized},
		{"wrong password", lanDirect, "", "admin", "wrong", http.StatusUnauthorized},
		{"wrong username", lanDirect, "", "root", "hunter2", http.StatusUnauthorized},
		{"public client direct with login", "203.0.113.7:40000", "", "admin", "hunter2", http.StatusForbidden},
		{"public client forging X-Forwarded-For", "203.0.113.7:40000", "192.168.1.50", "admin", "hunter2", http.StatusForbidden},
		{"LAN client via proxy with login", proxy, "192.168.1.50", "admin", "hunter2", http.StatusOK},
		{"LAN client via proxy without login", proxy, "192.168.1.50", "", "", http.StatusUnauthorized},
		{"public client via proxy with login", proxy, "203.0.113.7", "admin", "hunter2", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := adminRequest(tc.remoteAddr, tc.xff)
			if tc.user != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
			if tc.want == http.StatusUnauthorized {
				assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Basic")
			}
		})
	}
}

func TestAdminOnly_BasicAuthStillNeedsAllowedCIDRs(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	p.adminAuth = &Credentials{Username: "admin", Password: "hunter2"}
	req := adminRequest("127.0.0.1:40000", "")
	req.SetBasicAuth("admin", "hunter2")
	rec := httptest.NewRecorder()

	p.Mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// A browser attaches Basic credentials to cross-site requests by itself, so a
// valid login alone must not let another website change shares.
func TestAdminOnly_RejectsCrossSiteWrites(t *testing.T) {
	p := newTrustedTestProxy(t)
	p.adminAuth = &Credentials{Username: "admin", Password: "hunter2"}
	mux := p.Mux()

	cases := []struct {
		name   string
		method string
		path   string
		site   string
		want   int
	}{
		{"cross-site create", http.MethodPost, "/admin/shares", "cross-site", http.StatusForbidden},
		{"cross-site revoke", http.MethodDelete, "/admin/shares/abc", "cross-site", http.StatusForbidden},
		{"same-origin create reaches the handler", http.MethodPost, "/admin/shares", "same-origin", http.StatusBadRequest},
		{"non-browser client reaches the handler", http.MethodPost, "/admin/shares", "", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("not json"))
			req.RemoteAddr = "192.168.1.50:40000"
			req.SetBasicAuth("admin", "hunter2")
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}
