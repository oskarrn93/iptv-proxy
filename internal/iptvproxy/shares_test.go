package iptvproxy

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShareStore_CreateAndLookup(t *testing.T) {
	store := newShareStore()
	feed := newTestService(t, "http://upstream.example.com/get.php")
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")

	token, expiresAt := store.create(feed, target, sessionKindLive, time.Hour, "Test Channel")
	require.NotEmpty(t, token)
	assert.WithinDuration(t, time.Now().Add(time.Hour), expiresAt, time.Second)

	sh, ok := store.lookup(token)
	require.True(t, ok)
	assert.Same(t, feed, sh.feed)
	assert.Equal(t, target.String(), sh.target.String())
	assert.Equal(t, sessionKindLive, sh.kind)
}

func TestShareStore_LookupUnknownToken(t *testing.T) {
	store := newShareStore()
	_, ok := store.lookup("does-not-exist")
	assert.False(t, ok)
}

func TestShareStore_ExpiredShareIsGone(t *testing.T) {
	store := newShareStore()
	feed := newTestService(t, "http://upstream.example.com/get.php")
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")

	token, _ := store.create(feed, target, sessionKindLive, -time.Second, "Test Channel")

	_, ok := store.lookup(token)
	assert.False(t, ok, "an already-expired share must not be servable")
}

func TestShareStore_CreateCapsAtMaxTTL(t *testing.T) {
	store := newShareStore()
	feed := newTestService(t, "http://upstream.example.com/get.php")
	target, _ := url.Parse("http://upstream.example.com/live/x/y/1.ts")

	_, expiresAt := store.create(feed, target, sessionKindLive, 365*24*time.Hour, "Test Channel")
	assert.WithinDuration(t, time.Now().Add(maxShareTTL), expiresAt, time.Second)
}

func TestParseChannelPath(t *testing.T) {
	prefix, username, password, rest, ok := parseChannelPath("/live/alice/secret/12345.ts")
	require.True(t, ok)
	assert.Equal(t, "live", prefix)
	assert.Equal(t, "alice", username)
	assert.Equal(t, "secret", password)
	assert.Equal(t, "12345.ts", rest)
}

func TestParseChannelPath_TooShort(t *testing.T) {
	_, _, _, _, ok := parseChannelPath("/alice/secret")
	assert.False(t, ok)
}
