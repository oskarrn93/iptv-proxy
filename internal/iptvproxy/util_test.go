package iptvproxy

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestBufferedCopy(t *testing.T) {
	rec := httptest.NewRecorder()
	src := strings.NewReader("hello world")

	n, err := bufferedCopy(rec, src)
	require.NoError(t, err)
	assert.Equal(t, int64(len("hello world")), n)
	assert.Equal(t, "hello world", rec.Body.String())
}
