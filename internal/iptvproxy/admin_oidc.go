package iptvproxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	oidcLoginPath    = "/admin/oidc/login"
	oidcCallbackPath = "/admin/oidc/callback"

	oidcSessionCookie = "iptv_admin_session"
	oidcFlowCookie    = "iptv_admin_oidc"

	oidcSessionTTL = 8 * time.Hour
	// oidcFlowTTL bounds how long a login may take between the redirect to the
	// identity provider and the callback.
	oidcFlowTTL = 10 * time.Minute
)

// OIDCConfig logs admin users in through an OpenID Connect provider with the
// authorization-code flow and PKCE. It replaces admin_auth.
type OIDCConfig struct {
	Issuer   string `yaml:"issuer" validate:"required,http_url"`
	ClientID string `yaml:"client_id" validate:"required"`
	// ClientSecret is only for providers that insist on one; PKCE makes a
	// public client (no secret) work.
	ClientSecret string `yaml:"client_secret"`
	// RedirectURL is where the provider sends the user back; register it with
	// the provider. It must be this proxy's /admin/oidc/callback as the
	// browser reaches it.
	RedirectURL string `yaml:"redirect_url" validate:"required,http_url"`
	// AllowedEmails lists who may use the admin UI. Without it every account
	// at the provider could, so it is required.
	AllowedEmails []string `yaml:"allowed_emails" validate:"required"`
}

// oidcIdentity is what the provider vouched for.
type oidcIdentity struct {
	Email         string
	EmailVerified bool
}

// oidcBackend is the identity provider side of the login, behind an interface
// so the cookie, state and allow-list handling can be tested without one.
type oidcBackend interface {
	authURL(ctx context.Context, state, nonce, verifier string) (string, error)
	exchange(ctx context.Context, code, verifier, nonce string) (oidcIdentity, error)
}

// adminOIDC holds the login's state: the backend, who may log in, and the key
// that signs its cookies. The key is random per process, so a restart ends
// every session — acceptable for an admin page and keeps nothing on disk.
type adminOIDC struct {
	backend oidcBackend
	allowed []string
	key     []byte
	now     func() time.Time
}

func newAdminOIDC(cfg *OIDCConfig) (*adminOIDC, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	allowed := make([]string, len(cfg.AllowedEmails))
	for i, e := range cfg.AllowedEmails {
		allowed[i] = strings.ToLower(strings.TrimSpace(e))
	}
	return &adminOIDC{backend: &oidcProvider{cfg: cfg}, allowed: allowed, key: key, now: time.Now}, nil
}

// oidcProvider is the real backend. The provider's discovery document is
// fetched on first use, not at startup, so an unreachable identity provider
// never keeps the proxy itself from starting.
type oidcProvider struct {
	cfg *OIDCConfig

	mu       sync.Mutex
	provider *oidc.Provider
}

func (p *oidcProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	provider, err := oidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	p.provider = provider
	return provider, nil
}

func (p *oidcProvider) oauth(provider *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email"},
	}
}

func (p *oidcProvider) authURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	provider, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return p.oauth(provider).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), nil
}

func (p *oidcProvider) exchange(ctx context.Context, code, verifier, nonce string) (oidcIdentity, error) {
	provider, err := p.discover(ctx)
	if err != nil {
		return oidcIdentity{}, err
	}
	token, err := p.oauth(provider).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return oidcIdentity{}, err
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return oidcIdentity{}, errors.New("token response has no id_token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return oidcIdentity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return oidcIdentity{}, errors.New("nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return oidcIdentity{}, err
	}
	return oidcIdentity{Email: claims.Email, EmailVerified: claims.EmailVerified}, nil
}

// sign returns value tamper-proofed as "<value>.<mac>". Both halves are
// base64url, whose alphabet has no '.', so verify can split on the last one.
func (o *adminOIDC) sign(value string) string {
	mac := hmac.New(sha256.New, o.key)
	mac.Write([]byte(value))
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(value)) + "." + enc.EncodeToString(mac.Sum(nil))
}

func (o *adminOIDC) verify(signed string) (string, bool) {
	enc := base64.RawURLEncoding
	payload, sig, ok := strings.Cut(signed, ".")
	if !ok {
		return "", false
	}
	value, err1 := enc.DecodeString(payload)
	got, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, o.key)
	mac.Write(value)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return "", false
	}
	return string(value), true
}

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform is broken; a login must not
		// proceed on predictable state.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o *adminOIDC) setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(requestBaseURL(r), "https://"),
	})
}

// session returns the email of a valid, unexpired session cookie.
func (o *adminOIDC) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(oidcSessionCookie)
	if err != nil {
		return "", false
	}
	value, ok := o.verify(c.Value)
	if !ok {
		return "", false
	}
	email, exp, ok := strings.Cut(value, "|")
	expires, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || o.now().Unix() >= expires {
		return "", false
	}
	return email, true
}

// authorize is adminOnly's login check. The two login routes are reachable
// without a session, since they are how one is obtained.
func (o *adminOIDC) authorize(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == oidcLoginPath || r.URL.Path == oidcCallbackPath {
		return true
	}
	if _, ok := o.session(r); ok {
		return true
	}
	if r.Method == http.MethodGet {
		http.Redirect(w, r, oidcLoginPath, http.StatusFound)
		return false
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (p *Proxy) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	o := p.oidc
	state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
	target, err := o.backend.authURL(r.Context(), state, nonce, verifier)
	if err != nil {
		loggerFromContext(r.Context(), p.logger).ErrorContext(r.Context(), "oidc login failed", "error", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	expires := o.now().Add(oidcFlowTTL).Unix()
	// Path covers only the callback so the flow cookie goes nowhere else.
	o.setCookie(w, r, oidcFlowCookie, o.sign(fmt.Sprintf("%s|%s|%s|%d", state, nonce, verifier, expires)), oidcCallbackPath, oidcFlowTTL)
	http.Redirect(w, r, target, http.StatusFound)
}

func (p *Proxy) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	o := p.oidc
	logger := loggerFromContext(r.Context(), p.logger)
	deny := func(msg string, args ...any) {
		logger.WarnContext(r.Context(), "oidc login denied", append([]any{"reason", msg}, args...)...)
		http.Error(w, "login failed", http.StatusForbidden)
	}

	c, err := r.Cookie(oidcFlowCookie)
	if err != nil {
		deny("no login in progress")
		return
	}
	value, ok := o.verify(c.Value)
	parts := strings.Split(value, "|")
	if !ok || len(parts) != 4 {
		deny("bad flow cookie")
		return
	}
	state, nonce, verifier := parts[0], parts[1], parts[2]
	if expires, err := strconv.ParseInt(parts[3], 10, 64); err != nil || o.now().Unix() >= expires {
		deny("login expired")
		return
	}
	// The flow cookie is single-use.
	o.setCookie(w, r, oidcFlowCookie, "", oidcCallbackPath, -time.Second)

	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		deny("state mismatch")
		return
	}
	if e := q.Get("error"); e != "" {
		deny("provider error", "error", e)
		return
	}

	id, err := o.backend.exchange(r.Context(), q.Get("code"), verifier, nonce)
	if err != nil {
		deny("token exchange failed", "error", err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(id.Email))
	if !id.EmailVerified || !slices.Contains(o.allowed, email) {
		deny("not allowed", "email", email)
		return
	}

	expires := o.now().Add(oidcSessionTTL).Unix()
	o.setCookie(w, r, oidcSessionCookie, o.sign(fmt.Sprintf("%s|%d", email, expires)), "/admin", oidcSessionTTL)
	logger.InfoContext(r.Context(), "admin login", "email", email)
	http.Redirect(w, r, "/admin", http.StatusFound)
}
