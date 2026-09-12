package iptvproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestProxyWithAdmin mirrors newTestProxy but also sets an admin token, so
// /admin/shares tests don't each have to spell out the Config literal.
func newTestProxyWithAdmin(t *testing.T, upstream, adminToken string) *Proxy {
	t.Helper()
	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds:      []FeedConfig{testFeedConfig(upstream)},
		AdminToken: adminToken,
	}, testLogger(), true)
	require.NoError(t, err)
	return p
}

func createShareRequestBody(feed, channelURL, ttl string) *strings.Reader {
	body, _ := json.Marshal(createShareRequest{Feed: feed, ChannelURL: channelURL, TTL: ttl})
	return strings.NewReader(string(body))
}

func TestHandleCreateShare_RequiresAdminToken(t *testing.T) {
	p := newTestProxyWithAdmin(t, "http://upstream.example.com/get.php", "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://x/live/alice/secret/1.ts", "1h"))
	rec := httptest.NewRecorder()
	p.handleCreateShare(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleCreateShare_DisabledWithoutConfiguredToken(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php") // no AdminToken set

	req := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://x/live/alice/secret/1.ts", "1h"))
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	p.handleCreateShare(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleCreateShare_RejectsCredentialsForWrongFeed(t *testing.T) {
	p := newTestProxyWithAdmin(t, "http://upstream.example.com/get.php", "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://x/live/mallory/wrong/1.ts", "1h"))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	p.handleCreateShare(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleCreateShare_MintsWorkingShare walks the whole feature end to end:
// an admin request bearing the real client-facing channel URL mints a share
// token, and a GET against that share, with no feed credentials at all,
// reaches the exact upstream URL the channel link stood for.
func TestHandleCreateShare_MintsWorkingShare(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live/xtreamuser/xtreampass/segment.ts" {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write([]byte{0x47, 0x01, 0x02, 0x03})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxyWithAdmin(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass", "s3cret")

	createReq := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://iptv.example.com/live/alice/secret/segment.ts", "1h"))
	createReq.Header.Set("Authorization", "Bearer s3cret")
	createRec := httptest.NewRecorder()
	p.handleCreateShare(createRec, createReq)
	require.Equal(t, http.StatusOK, createRec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &resp))
	require.Contains(t, resp["url"], "/share/")
	token := strings.TrimPrefix(resp["url"], "http://example.com/share/")

	shareReq := httptest.NewRequest(http.MethodGet, "/share/"+token, nil)
	shareReq.SetPathValue("token", token)
	shareRec := httptest.NewRecorder()
	p.handleShare(shareRec, shareReq)

	require.Equal(t, http.StatusOK, shareRec.Code)
	assert.Equal(t, []byte{0x47, 0x01, 0x02, 0x03}, shareRec.Body.Bytes())
}

func TestHandleShare_UnknownOrExpiredToken(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodGet, "/share/does-not-exist", nil)
	req.SetPathValue("token", "does-not-exist")
	rec := httptest.NewRecorder()
	p.handleShare(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleShare_RejectsForeignHost checks the same SSRF-closing property
// handleStream already has (see TestHandleStream_CannotReachAnotherHost): a
// share's own "u" query param can only ever point back at the upstream host
// the share was created against, never an arbitrary one.
func TestHandleShare_RejectsForeignHost(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	feed := p.feeds[0]
	target, err := url.Parse("http://upstream.example.com/live/x/y/1.ts")
	require.NoError(t, err)
	token, _ := p.shares.create(feed, target, sessionKindLive, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/share/"+token+"?u=http://evil.example.com/secret", nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleShare(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
