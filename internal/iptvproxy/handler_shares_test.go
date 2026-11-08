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

func createShareRequestBody(feed, channelURL, ttl string) *strings.Reader {
	body, _ := json.Marshal(createShareRequest{Feed: feed, ChannelURL: channelURL, TTL: ttl})
	return strings.NewReader(string(body))
}

func TestHandleCreateShare_RejectsCredentialsForWrongFeed(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://x/live/mallory/wrong/1.ts", "1h"))
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

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	createReq := httptest.NewRequest(http.MethodPost, "/admin/shares",
		createShareRequestBody("test", "http://iptv.example.com/live/alice/secret/segment.ts", "1h"))
	createRec := httptest.NewRecorder()
	p.handleCreateShare(createRec, createReq)
	require.Equal(t, http.StatusOK, createRec.Code)

	body := createRec.Body.String()
	const marker = `href="`
	start := strings.Index(body, marker) + len(marker)
	require.Greater(t, start, len(marker)-1, "response should contain a share link: %s", body)
	shareURL := body[start : strings.Index(body[start:], `"`)+start]
	require.Contains(t, shareURL, "/share/")
	token := strings.TrimPrefix(shareURL, "http://example.com/share/")

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
// handleStream already has (see TestHandleStream_CannotReachAnotherHost): even
// a correctly sealed ref can only ever point back at the upstream host the
// share was created against, never an arbitrary one.
func TestHandleShare_RejectsForeignHost(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	feed := p.feeds[0]
	target, err := url.Parse("http://upstream.example.com/live/x/y/1.ts")
	require.NoError(t, err)
	token, _ := p.shares.create(feed, target, sessionKindLive, time.Hour, "Test Channel")
	sh, ok := p.shares.lookup(token)
	require.True(t, ok)
	evil, _ := url.Parse("http://evil.example.com/secret")

	req := httptest.NewRequest(http.MethodGet, "/share/"+token+"?ref="+sh.sealRef(evil), nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleShare(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleShare_RejectsForgedOrForeignRef: a viewer can't write their own
// ref, and a ref sealed for one share doesn't open against another.
func TestHandleShare_RejectsForgedOrForeignRef(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	feed := p.feeds[0]
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")
	tokenA, _ := p.shares.create(feed, target, sessionKindLive, time.Hour, "A")
	tokenB, _ := p.shares.create(feed, target, sessionKindLive, time.Hour, "B")
	shB, _ := p.shares.lookup(tokenB)
	other, _ := url.Parse("http://upstream.example.com/live/x/y/2.ts")

	for name, ref := range map[string]string{
		"plain url":         url.QueryEscape(other.String()),
		"garbage":           "AAAA",
		"other share's ref": shB.sealRef(other),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/share/"+tokenA+"?ref="+ref, nil)
			req.SetPathValue("token", tokenA)
			rec := httptest.NewRecorder()
			p.handleShare(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestHandleShare_PlaylistDoesNotLeakUpstream is the regression test for share
// playlists once carrying each nested upstream URL — real Xtream username and
// password in its path — in plain text. Following a rewritten ref must still
// reach the right upstream path.
func TestHandleShare_PlaylistDoesNotLeakUpstream(t *testing.T) {
	var segmentHits int
	var upstreamHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live/xtreamuser/xtreampass/1.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n720p.m3u8\n#EXTINF:4,\nseg1.ts\n"))
		case "/live/xtreamuser/xtreampass/seg1.ts":
			segmentHits++
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write([]byte{0x47, 0x00})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	upstreamHost = strings.TrimPrefix(upstream.URL, "http://")

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")
	target, _ := url.Parse(upstream.URL + "/live/xtreamuser/xtreampass/1.m3u8")
	token, _ := p.shares.create(p.feeds[0], target, sessionKindLive, time.Hour, "x")

	req := httptest.NewRequest(http.MethodGet, "/share/"+token, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleShare(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	for _, secret := range []string{"xtreamuser", "xtreampass", upstreamHost, url.QueryEscape(upstreamHost)} {
		assert.NotContains(t, body, secret)
	}

	var segLink string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "/share/") && !strings.HasPrefix(line, "#") {
			segLink = line // last URI line is seg1.ts
		}
	}
	require.NotEmpty(t, segLink)
	segURL, err := url.Parse(segLink)
	require.NoError(t, err)

	segReq := httptest.NewRequest(http.MethodGet, segURL.RequestURI(), nil)
	segReq.SetPathValue("token", token)
	segRec := httptest.NewRecorder()
	p.handleShare(segRec, segReq)
	assert.Equal(t, http.StatusOK, segRec.Code)
	assert.Equal(t, 1, segmentHits)
}

func TestHandleRevokeShare_DropsShareImmediately(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	feed := p.feeds[0]
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")
	token, _ := p.shares.create(feed, target, sessionKindLive, time.Hour, "Test Channel")

	req := httptest.NewRequest(http.MethodDelete, "/admin/shares/"+token, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleRevokeShare(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	_, ok := p.shares.lookup(token)
	assert.False(t, ok, "a revoked share must no longer be servable")
}

func TestHandleRevokeShare_UnknownToken(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodDelete, "/admin/shares/does-not-exist", nil)
	req.SetPathValue("token", "does-not-exist")
	rec := httptest.NewRecorder()
	p.handleRevokeShare(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleListShares_RendersActiveShares(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	feed := p.feeds[0]
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")
	token, _ := p.shares.create(feed, target, sessionKindLive, time.Hour, "Test Channel")

	req := httptest.NewRequest(http.MethodGet, "/admin/shares", nil)
	rec := httptest.NewRecorder()
	p.handleListShares(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), token)
	assert.Contains(t, rec.Body.String(), "test") // feed name
}

func TestHandleAdminFeeds_ListsConfiguredFeeds(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodGet, "/admin/feeds", nil)
	rec := httptest.NewRecorder()
	p.handleAdminFeeds(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `<option value="test">test</option>`)
}

func TestHandleAdminChannels_ListsChannelsFromRewrittenPlaylist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Channel One\nhttp://upstream.example.com/live/xtreamuser/xtreampass/1.ts\n"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	req := httptest.NewRequest(http.MethodGet, "/admin/channels?feed=test", nil)
	rec := httptest.NewRecorder()
	p.handleAdminChannels(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Channel One")
	assert.Contains(t, rec.Body.String(), "/live/alice/secret/1.ts")
}

func TestHandleAdminChannels_UnknownFeed(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodGet, "/admin/channels?feed=nope", nil)
	rec := httptest.NewRecorder()
	p.handleAdminChannels(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
