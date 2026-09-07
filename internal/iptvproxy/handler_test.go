package iptvproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds: []FeedConfig{{
			Name:        "test",
			UpstreamURL: upstream,
			Username:    "alice",
			Password:    "secret",
			Timeout:     5 * time.Second,
		}},
	}, testLogger(), true)
	require.NoError(t, err)
	return p
}

func TestNewProxy_InvalidUpstreamURL(t *testing.T) {
	_, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds:      []FeedConfig{{Name: "f", UpstreamURL: "not a url", Username: "a", Password: "b"}},
	}, testLogger(), true)
	assert.Error(t, err)
}

func TestAuthenticate(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	t.Run("valid query params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		assert.NotNil(t, p.authenticate(rec, req))
	})

	t.Run("valid basic auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		req.SetBasicAuth("alice", "secret")
		rec := httptest.NewRecorder()
		assert.NotNil(t, p.authenticate(rec, req))
	})

	t.Run("wrong password", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=wrong", nil)
		rec := httptest.NewRecorder()
		assert.Nil(t, p.authenticate(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		assert.Nil(t, p.authenticate(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestAuthenticate_MultipleFeeds(t *testing.T) {
	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds: []FeedConfig{
			{Name: "feed-1", UpstreamURL: "http://upstream1.example.com/get.php", Username: "alice", Password: "secret1"},
			{Name: "feed-2", UpstreamURL: "http://upstream2.example.com/get.php", Username: "bob", Password: "secret2"},
		},
	}, testLogger(), true)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=bob&password=secret2", nil)
	rec := httptest.NewRecorder()
	f := p.authenticate(rec, req)
	require.NotNil(t, f)
	assert.Equal(t, "feed-2", f.name)
}

func TestHandlePlaylist_RewritesAndRequiresAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	t.Run("unauthorized without creds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		p.handlePlaylist(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("rewrites playlist for authorized request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		p.handlePlaylist(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		assert.Contains(t, body, "/stream?u=")
		assert.NotContains(t, body, upstream.URL)
	})
}

func TestHandleStream_ForbidsPrivateTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	target := url.QueryEscape("http://127.0.0.1:9/secret")
	req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u="+target, nil)
	rec := httptest.NewRecorder()
	p.handleStream(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleStream_MissingOrBadTarget(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	t.Run("missing u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unparseable u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u=%zz", nil)
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("relative u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u=relative.ts", nil)
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestHandleStream_ProxiesSegment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/segment.ts" {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write([]byte{0x47, 0x01, 0x02, 0x03})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	target := url.QueryEscape(upstream.URL + "/segment.ts")
	req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u="+target, nil)
	rec := httptest.NewRecorder()
	p.handleStream(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte{0x47, 0x01, 0x02, 0x03}, rec.Body.Bytes())
}

func TestHandlePlaylist_ForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")
	req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	p.handlePlaylist(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHealthEndpoint(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestMetricsEndpoint(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	mux := p.Mux()

	// Generate an auth failure so a feed-labeled sample exists to assert on:
	// a recognized username with the wrong password attributes to "test".
	authReq := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=wrong", nil)
	mux.ServeHTTP(httptest.NewRecorder(), authReq)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `feed="test"`)
	assert.Contains(t, body, "iptv_proxy_auth_failures_total")
}

func TestMetricsEndpoint_Disabled(t *testing.T) {
	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds: []FeedConfig{{
			Name:        "test",
			UpstreamURL: "http://upstream.example.com/get.php",
			Username:    "alice",
			Password:    "secret",
			Timeout:     5 * time.Second,
		}},
	}, testLogger(), false)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
