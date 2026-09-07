package iptvproxy

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T, upstream string) *feedService {
	t.Helper()
	cfg := FeedConfig{
		Name:        "test",
		ListenAddr:  ":0",
		UpstreamURL: upstream,
		Username:    "alice",
		Password:    "secret",
		Timeout:     5 * time.Second,
	}
	s, err := newFeedService(cfg, newUpstreamRepository(testLogger()), testLogger())
	require.NoError(t, err)
	return s
}

func TestNewFeedService_InvalidUpstreamURL(t *testing.T) {
	tests := []string{
		"",
		"not a url",
		"ftp://example.com",
		"example.com/get.php", // opaque, no scheme
	}
	for _, u := range tests {
		t.Run(u, func(t *testing.T) {
			_, err := newFeedService(FeedConfig{Name: "f", UpstreamURL: u, Username: "a", Password: "b"}, newUpstreamRepository(testLogger()), testLogger())
			assert.Error(t, err)
		})
	}
}

func TestNewFeedService_ValidUpstreamURL(t *testing.T) {
	s := newTestService(t, "http://example.com/get.php?type=m3u")
	assert.Equal(t, "example.com", s.target.Host)
	assert.Equal(t, "username=alice&password=secret", s.creds)
}

func TestRewritePlaylist(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\nsegment1.ts\n#EXTINF:10,\nhttp://other.com/segment2.ts\n")

	out, err := s.rewritePlaylist(body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "#EXTM3U")
	assert.Contains(t, out, "/stream?u="+url.QueryEscape("http://upstream.example.com/live/segment1.ts")+"&username=alice&password=secret")
	assert.Contains(t, out, "/stream?u="+url.QueryEscape("http://other.com/segment2.ts")+"&username=alice&password=secret")
	assert.NotContains(t, out, "upstream.example.com/live/segment1.ts\n") // raw URI not left unrewritten
}

func TestRewritePlaylist_KeyAndMapTags(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte(`#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234
#EXT-X-MAP:URI="init.mp4"
segment1.ts
`)

	out, err := s.rewritePlaylist(body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "URI=\"/stream?u="+url.QueryEscape("http://upstream.example.com/live/key.bin")+"&username=alice&password=secret\"")
	assert.Contains(t, out, "URI=\"/stream?u="+url.QueryEscape("http://upstream.example.com/live/init.mp4")+"&username=alice&password=secret\"")
	assert.Contains(t, out, "METHOD=AES-128")
	assert.Contains(t, out, "IV=0x1234")
}

func TestRewriteAttrURI_SingleQuotes(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	base, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	line := `#EXT-X-KEY:METHOD=AES-128,URI='key.bin'`
	out := s.rewriteAttrURI(line, base)
	assert.Contains(t, out, "URI='/stream?u=")
	assert.Contains(t, out, "&username=alice&password=secret'")
}

func TestValidateStreamTarget(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	ctx := context.Background()

	t.Run("matches configured upstream host", func(t *testing.T) {
		target, _ := url.Parse("http://upstream.example.com/live/seg.ts")
		assert.NoError(t, s.validateStreamTarget(ctx, target))
	})

	t.Run("rejects non-http(s) scheme", func(t *testing.T) {
		target, _ := url.Parse("ftp://upstream.example.com/x")
		assert.Error(t, s.validateStreamTarget(ctx, target))
	})

	t.Run("rejects loopback address", func(t *testing.T) {
		target, _ := url.Parse("http://127.0.0.1/admin")
		assert.Error(t, s.validateStreamTarget(ctx, target))
	})

	t.Run("rejects private address", func(t *testing.T) {
		target, _ := url.Parse("http://192.168.1.12/admin")
		assert.Error(t, s.validateStreamTarget(ctx, target))
	})

	t.Run("rejects link-local address", func(t *testing.T) {
		target, _ := url.Parse("http://169.254.169.254/latest/meta-data")
		assert.Error(t, s.validateStreamTarget(ctx, target))
	})

	t.Run("allows a public address for a different host", func(t *testing.T) {
		target, _ := url.Parse("http://1.1.1.1/x")
		assert.NoError(t, s.validateStreamTarget(ctx, target))
	})
}

func TestResolveStreamTarget(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")

	t.Run("valid absolute URL", func(t *testing.T) {
		target, err := s.resolveStreamTarget("http://upstream.example.com/live/seg.ts")
		require.NoError(t, err)
		assert.Equal(t, "upstream.example.com", target.Host)
	})

	t.Run("unparseable", func(t *testing.T) {
		_, err := s.resolveStreamTarget("%zz")
		assert.Error(t, err)
	})

	t.Run("relative", func(t *testing.T) {
		_, err := s.resolveStreamTarget("relative.ts")
		assert.Error(t, err)
	})
}
