package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestProxy(t *testing.T, upstream string) *feedProxy {
	t.Helper()
	p, err := newFeedProxy(FeedConfig{
		Name:        "test",
		ListenAddr:  ":0",
		UpstreamURL: upstream,
		Username:    "alice",
		Password:    "secret",
		Timeout:     5 * time.Second,
	})
	require.NoError(t, err)
	return p
}

func TestNewFeedProxy_InvalidUpstreamURL(t *testing.T) {
	tests := []string{
		"",
		"not a url",
		"ftp://example.com",
		"example.com/get.php", // opaque, no scheme
	}
	for _, u := range tests {
		t.Run(u, func(t *testing.T) {
			_, err := newFeedProxy(FeedConfig{Name: "f", UpstreamURL: u, Username: "a", Password: "b"})
			assert.Error(t, err)
		})
	}
}

func TestNewFeedProxy_ValidUpstreamURL(t *testing.T) {
	p := newTestProxy(t, "http://example.com/get.php?type=m3u")
	assert.Equal(t, "example.com", p.target.Host)
	assert.Equal(t, "username=alice&password=secret", p.creds)
}

func TestRedact(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			in:   "http://host/get.php?username=alice&password=hunter2",
			want: "http://host/get.php?username=REDACTED&password=REDACTED",
		},
		{
			in:   "http://host/x?token=abc123&other=keep",
			want: "http://host/x?token=REDACTED&other=keep",
		},
		{
			in:   "no secrets here",
			want: "no secrets here",
		},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, redact(tt.in))
	}
}

func TestRedactErr(t *testing.T) {
	assert.Equal(t, "<nil>", redactErr(nil))
	err := errors.New("failed fetching http://host/x?password=hunter2")
	assert.Equal(t, "failed fetching http://host/x?password=REDACTED", redactErr(err))
}

func TestIsTransientError(t *testing.T) {
	assert.True(t, isTransientError(context.DeadlineExceeded))
	assert.True(t, isTransientError(io.EOF))
	assert.True(t, isTransientError(io.ErrUnexpectedEOF))
	assert.False(t, isTransientError(context.Canceled))
	assert.False(t, isTransientError(errors.New("some other error")))
}

func TestIsRetryableStatus(t *testing.T) {
	assert.True(t, isRetryableStatus(http.StatusTooManyRequests))
	assert.True(t, isRetryableStatus(http.StatusInternalServerError))
	assert.True(t, isRetryableStatus(http.StatusBadGateway))
	assert.True(t, isRetryableStatus(http.StatusServiceUnavailable))
	assert.True(t, isRetryableStatus(http.StatusGatewayTimeout))
	assert.False(t, isRetryableStatus(http.StatusOK))
	assert.False(t, isRetryableStatus(http.StatusNotFound))
	assert.False(t, isRetryableStatus(http.StatusUnauthorized))
}

func TestResolveURI(t *testing.T) {
	base, err := url.Parse("http://example.com/live/playlist.m3u8")
	require.NoError(t, err)

	resolved, err := resolveURI(base, "segment1.ts")
	require.NoError(t, err)
	assert.Equal(t, "http://example.com/live/segment1.ts", resolved.String())

	resolved, err = resolveURI(base, "http://other.com/abs.ts")
	require.NoError(t, err)
	assert.Equal(t, "http://other.com/abs.ts", resolved.String())
}

func TestShouldRewritePlaylist(t *testing.T) {
	m3u8URL, _ := url.Parse("http://example.com/playlist.m3u8")
	otherURL, _ := url.Parse("http://example.com/get.php")

	assert.True(t, shouldRewritePlaylist(m3u8URL, "", nil))
	assert.True(t, shouldRewritePlaylist(otherURL, "application/vnd.apple.mpegurl", nil))
	assert.True(t, shouldRewritePlaylist(otherURL, "application/x-mpegURL", nil))
	assert.True(t, shouldRewritePlaylist(otherURL, "text/plain", []byte("#EXTM3U\n#EXTINF")))
	assert.False(t, shouldRewritePlaylist(otherURL, "video/mp2t", []byte{0x47, 0x00}))
}

func TestIsTSPath(t *testing.T) {
	tsPath, _ := url.Parse("http://example.com/ts/stream/12345")
	tsExt, _ := url.Parse("http://example.com/segment.ts")
	other, _ := url.Parse("http://example.com/playlist.m3u8")

	assert.True(t, isTSPath(tsPath))
	assert.True(t, isTSPath(tsExt))
	assert.False(t, isTSPath(other))
}

func TestIsTSStream(t *testing.T) {
	other, _ := url.Parse("http://example.com/get.php")

	assert.True(t, isTSStream(other, "video/mp2t", nil))
	assert.True(t, isTSStream(other, "video/ts", nil))
	assert.True(t, isTSStream(other, "", []byte{0x47, 0x01}))
	assert.False(t, isTSStream(other, "application/vnd.apple.mpegurl", []byte("#EXTM3U")))
}

func TestRewritePlaylist(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\nsegment1.ts\n#EXTINF:10,\nhttp://other.com/segment2.ts\n")

	out, err := p.rewritePlaylist(body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "#EXTM3U")
	assert.Contains(t, out, "/stream?u="+url.QueryEscape("http://upstream.example.com/live/segment1.ts")+"&username=alice&password=secret")
	assert.Contains(t, out, "/stream?u="+url.QueryEscape("http://other.com/segment2.ts")+"&username=alice&password=secret")
	assert.NotContains(t, out, "upstream.example.com/live/segment1.ts\n") // raw URI not left unrewritten
}

func TestRewritePlaylist_KeyAndMapTags(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte(`#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234
#EXT-X-MAP:URI="init.mp4"
segment1.ts
`)

	out, err := p.rewritePlaylist(body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "URI=\"/stream?u="+url.QueryEscape("http://upstream.example.com/live/key.bin")+"&username=alice&password=secret\"")
	assert.Contains(t, out, "URI=\"/stream?u="+url.QueryEscape("http://upstream.example.com/live/init.mp4")+"&username=alice&password=secret\"")
	assert.Contains(t, out, "METHOD=AES-128")
	assert.Contains(t, out, "IV=0x1234")
}

func TestRewriteAttrURI_SingleQuotes(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	base, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	line := `#EXT-X-KEY:METHOD=AES-128,URI='key.bin'`
	out := p.rewriteAttrURI(line, base)
	assert.Contains(t, out, "URI='/stream?u=")
	assert.Contains(t, out, "&username=alice&password=secret'")
}

func TestValidateStreamTarget(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	ctx := context.Background()

	t.Run("matches configured upstream host", func(t *testing.T) {
		target, _ := url.Parse("http://upstream.example.com/live/seg.ts")
		assert.NoError(t, p.validateStreamTarget(ctx, target))
	})

	t.Run("rejects non-http(s) scheme", func(t *testing.T) {
		target, _ := url.Parse("ftp://upstream.example.com/x")
		assert.Error(t, p.validateStreamTarget(ctx, target))
	})

	t.Run("rejects loopback address", func(t *testing.T) {
		target, _ := url.Parse("http://127.0.0.1/admin")
		assert.Error(t, p.validateStreamTarget(ctx, target))
	})

	t.Run("rejects private address", func(t *testing.T) {
		target, _ := url.Parse("http://192.168.1.12/admin")
		assert.Error(t, p.validateStreamTarget(ctx, target))
	})

	t.Run("rejects link-local address", func(t *testing.T) {
		target, _ := url.Parse("http://169.254.169.254/latest/meta-data")
		assert.Error(t, p.validateStreamTarget(ctx, target))
	})

	t.Run("allows a public address for a different host", func(t *testing.T) {
		target, _ := url.Parse("http://1.1.1.1/x")
		assert.NoError(t, p.validateStreamTarget(ctx, target))
	})
}

func TestRequireAuth(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	t.Run("valid query params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		assert.True(t, p.requireAuth(rec, req))
	})

	t.Run("valid basic auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		req.SetBasicAuth("alice", "secret")
		rec := httptest.NewRecorder()
		assert.True(t, p.requireAuth(rec, req))
	})

	t.Run("wrong password", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=wrong", nil)
		rec := httptest.NewRecorder()
		assert.False(t, p.requireAuth(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		assert.False(t, p.requireAuth(rec, req))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
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

func TestProxyTarget_ForwardsUpstreamErrorStatus(t *testing.T) {
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
	p.mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestBufferedCopy(t *testing.T) {
	rec := httptest.NewRecorder()
	src := strings.NewReader("hello world")

	n, err := bufferedCopy(rec, src)
	require.NoError(t, err)
	assert.Equal(t, int64(len("hello world")), n)
	assert.Equal(t, "hello world", rec.Body.String())
}
