package iptvproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOIDC stands in for the identity provider: it records what the login
// asked for and answers the code exchange with a canned identity.
type fakeOIDC struct {
	identity               oidcIdentity
	err                    error
	state, nonce, verifier string
	gotCode, gotVerifier   string
	gotNonce               string
}

func (f *fakeOIDC) authURL(_ context.Context, state, nonce, verifier string) (string, error) {
	f.state, f.nonce, f.verifier = state, nonce, verifier
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}

func (f *fakeOIDC) exchange(_ context.Context, code, verifier, nonce string) (oidcIdentity, error) {
	f.gotCode, f.gotVerifier, f.gotNonce = code, verifier, nonce
	return f.identity, f.err
}

func newOIDCProxy(t *testing.T, id oidcIdentity) (*Proxy, *fakeOIDC) {
	t.Helper()
	fake := &fakeOIDC{identity: id}
	p, err := NewProxy(&Config{
		ListenAddr:        ":0",
		Feeds:             []FeedConfig{testFeedConfig("http://upstream.example.com/get.php")},
		AdminAllowedCIDRs: []string{"0.0.0.0/0"},
		AdminOIDC: &OIDCConfig{
			Issuer: "https://idp.example", ClientID: "c", RedirectURL: "https://proxy.example/admin/oidc/callback",
			AllowedEmails: []string{"Admin@Example.com"},
		},
	}, testLogger(), false)
	require.NoError(t, err)
	p.oidc.backend = fake
	return p, fake
}

func do(p *Proxy, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "10.0.0.5:1234"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, req)
	return rec
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", name)
	return nil
}

// login walks the whole flow and returns the callback response.
func login(t *testing.T, p *Proxy, fake *fakeOIDC) *httptest.ResponseRecorder {
	t.Helper()
	start := do(p, http.MethodGet, oidcLoginPath)
	require.Equal(t, http.StatusFound, start.Code)
	flow := cookieNamed(t, start, oidcFlowCookie)
	return do(p, http.MethodGet, oidcCallbackPath+"?code=abc&state="+url.QueryEscape(fake.state), flow)
}

func TestAdminOIDC_FullLoginGrantsSession(t *testing.T) {
	p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})

	assert.Equal(t, http.StatusFound, do(p, http.MethodGet, "/admin").Code)
	assert.Equal(t, oidcLoginPath, do(p, http.MethodGet, "/admin").Header().Get("Location"), "no session redirects to login")
	assert.Equal(t, http.StatusUnauthorized, do(p, http.MethodDelete, "/admin/shares/x").Code)

	start := do(p, http.MethodGet, oidcLoginPath)
	assert.Contains(t, start.Header().Get("Location"), "https://idp.example/authorize")
	flow := cookieNamed(t, start, oidcFlowCookie)
	assert.True(t, flow.HttpOnly)
	assert.Equal(t, oidcCallbackPath, flow.Path)

	cb := do(p, http.MethodGet, oidcCallbackPath+"?code=abc&state="+url.QueryEscape(fake.state), flow)
	require.Equal(t, http.StatusFound, cb.Code, cb.Body.String())
	assert.Equal(t, "/admin", cb.Header().Get("Location"))
	assert.Equal(t, "abc", fake.gotCode)
	assert.Equal(t, fake.verifier, fake.gotVerifier, "the PKCE verifier from the login reaches the exchange")
	assert.Equal(t, fake.nonce, fake.gotNonce)

	session := cookieNamed(t, cb, oidcSessionCookie)
	assert.True(t, session.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, session.SameSite)
	assert.Equal(t, http.StatusOK, do(p, http.MethodGet, "/admin", session).Code)
}

func TestAdminOIDC_Rejections(t *testing.T) {
	t.Run("email not allowed", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{Email: "mallory@example.com", EmailVerified: true})
		assert.Equal(t, http.StatusForbidden, login(t, p, fake).Code)
	})
	t.Run("email not verified", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com"})
		assert.Equal(t, http.StatusForbidden, login(t, p, fake).Code)
	})
	t.Run("exchange fails", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{})
		fake.err = errors.New("boom")
		assert.Equal(t, http.StatusForbidden, login(t, p, fake).Code)
	})
	t.Run("state mismatch", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})
		flow := cookieNamed(t, do(p, http.MethodGet, oidcLoginPath), oidcFlowCookie)
		assert.Equal(t, http.StatusForbidden, do(p, http.MethodGet, oidcCallbackPath+"?code=abc&state=forged", flow).Code)
		assert.Empty(t, fake.gotCode, "no code is exchanged for a forged state")
	})
	t.Run("no flow cookie", func(t *testing.T) {
		p, _ := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})
		assert.Equal(t, http.StatusForbidden, do(p, http.MethodGet, oidcCallbackPath+"?code=abc&state=x").Code)
	})
	t.Run("tampered flow cookie", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})
		flow := cookieNamed(t, do(p, http.MethodGet, oidcLoginPath), oidcFlowCookie)
		flow.Value += "x"
		assert.Equal(t, http.StatusForbidden, do(p, http.MethodGet, oidcCallbackPath+"?code=abc&state="+fake.state, flow).Code)
	})
	t.Run("provider error", func(t *testing.T) {
		p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})
		flow := cookieNamed(t, do(p, http.MethodGet, oidcLoginPath), oidcFlowCookie)
		assert.Equal(t, http.StatusForbidden, do(p, http.MethodGet, oidcCallbackPath+"?error=access_denied&state="+fake.state, flow).Code)
	})
}

func TestAdminOIDC_SessionIsSignedAndExpires(t *testing.T) {
	p, fake := newOIDCProxy(t, oidcIdentity{Email: "admin@example.com", EmailVerified: true})
	session := cookieNamed(t, login(t, p, fake), oidcSessionCookie)

	forged := *session
	forged.Value = p.oidc.sign("admin@example.com|99999999999")[:10] + "x"
	assert.Equal(t, http.StatusFound, do(p, http.MethodGet, "/admin", &forged).Code)

	other, err := newAdminOIDC(&OIDCConfig{AllowedEmails: []string{"a@b"}})
	require.NoError(t, err)
	foreign := *session
	foreign.Value = other.sign("admin@example.com|99999999999")
	assert.Equal(t, http.StatusFound, do(p, http.MethodGet, "/admin", &foreign).Code, "a cookie signed with another key is refused")

	p.oidc.now = func() time.Time { return time.Now().Add(oidcSessionTTL + time.Minute) }
	assert.Equal(t, http.StatusFound, do(p, http.MethodGet, "/admin", session).Code, "an expired session is refused")
}

func TestAdminOIDC_StillHonorsAllowedCIDRs(t *testing.T) {
	p, _ := newOIDCProxy(t, oidcIdentity{})
	p.trust.adminAllowed = nil
	assert.Equal(t, http.StatusForbidden, do(p, http.MethodGet, oidcLoginPath).Code)
}

func TestNewProxy_OIDCAndBasicAuthAreExclusive(t *testing.T) {
	_, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds:      []FeedConfig{testFeedConfig("http://upstream.example.com/get.php")},
		AdminAuth:  &Credentials{Username: "a", Password: "b"},
		AdminOIDC:  &OIDCConfig{Issuer: "https://idp.example", ClientID: "c", RedirectURL: "https://x/cb", AllowedEmails: []string{"a@b"}},
	}, testLogger(), false)
	assert.ErrorContains(t, err, "mutually exclusive")
}
