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

func newTestHandler(t *testing.T, upstream string) *FeedHandler {
	t.Helper()
	h, err := NewFeedProxy(FeedConfig{
		Name:        "test",
		ListenAddr:  ":0",
		UpstreamURL: upstream,
		Username:    "alice",
		Password:    "secret",
		Timeout:     5 * time.Second,
	}, testLogger())
	require.NoError(t, err)
	return h
}

func TestNewFeedProxy_InvalidUpstreamURL(t *testing.T) {
	_, err := NewFeedProxy(FeedConfig{Name: "f", UpstreamURL: "not a url", Username: "a", Password: "b"}, testLogger())
	assert.Error(t, err)
}

func TestRequireAuth(t *testing.T) {
	h := newTestHandler(t, "http://upstream.example.com/get.php")

	t.Run("valid query params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		assert.True(t, h.requireAuth(rec, req))
	})

	t.Run("valid basic auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		req.SetBasicAuth("alice", "secret")
		rec := httptest.NewRecorder()
		assert.True(t, h.requireAuth(rec, req))
	})

	t.Run("wrong password", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=wrong", nil)
		rec := httptest.NewRecorder()
		assert.False(t, h.requireAuth(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		assert.False(t, h.requireAuth(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestHandlePlaylist_RewritesAndRequiresAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL+"/get.php")

	t.Run("unauthorized without creds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		h.handlePlaylist(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("rewrites playlist for authorized request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		h.handlePlaylist(rec, req)

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

	h := newTestHandler(t, upstream.URL+"/get.php")

	target := url.QueryEscape("http://127.0.0.1:9/secret")
	req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u="+target, nil)
	rec := httptest.NewRecorder()
	h.handleStream(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleStream_MissingOrBadTarget(t *testing.T) {
	h := newTestHandler(t, "http://upstream.example.com/get.php")

	t.Run("missing u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		h.handleStream(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unparseable u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u=%zz", nil)
		rec := httptest.NewRecorder()
		h.handleStream(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("relative u param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u=relative.ts", nil)
		rec := httptest.NewRecorder()
		h.handleStream(rec, req)
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

	h := newTestHandler(t, upstream.URL+"/get.php")

	target := url.QueryEscape(upstream.URL + "/segment.ts")
	req := httptest.NewRequest(http.MethodGet, "/stream?username=alice&password=secret&u="+target, nil)
	rec := httptest.NewRecorder()
	h.handleStream(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte{0x47, 0x01, 0x02, 0x03}, rec.Body.Bytes())
}

func TestHandlePlaylist_ForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL+"/get.php")
	req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	h.handlePlaylist(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHealthEndpoint(t *testing.T) {
	h := newTestHandler(t, "http://upstream.example.com/get.php")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}
