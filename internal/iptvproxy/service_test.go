package iptvproxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFeedConfig is the config every test feed starts from; the "alice"/
// "secret" pair it carries is what resolveStreamLinks asserts rewritten links
// contain.
func testFeedConfig(upstream string) FeedConfig {
	return FeedConfig{
		Name:        "test",
		UpstreamURL: upstream,
		Credentials: Credentials{Username: "alice", Password: "secret"},
		Timeout:     5 * time.Second,
	}
}

// buildService constructs a feedService from cfg, wiring up the repository,
// logger, and metrics every caller would otherwise spell out identically.
func buildService(cfg FeedConfig) (*feedService, error) {
	return newFeedService(cfg, newUpstreamRepository(testLogger(), cfg.Timeout), testLogger(), newFeedMetrics(cfg.Name, true, testLogger()))
}

func newServiceFor(t *testing.T, cfg FeedConfig) *feedService {
	t.Helper()
	s, err := buildService(cfg)
	require.NoError(t, err)
	return s
}

func newTestService(t *testing.T, upstream string) *feedService {
	t.Helper()
	return newServiceFor(t, testFeedConfig(upstream))
}

// rewriteBody rewrites a playlist held in memory, adapting it to
// rewritePlaylist's streaming signature.
func rewriteBody(s *feedService, body []byte, srcURL *url.URL) (string, error) {
	return s.rewritePlaylist(bytes.NewReader(body), srcURL, int64(len(body)))
}

// newCountingPlaylistUpstream serves a fixed playlist and counts how many
// times it was actually fetched, so cache behavior can be asserted on.
func newCountingPlaylistUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// streamLinkPattern extracts the prefix/username/password/rest path segments
// from one rewritten stream link, e.g. "/live/alice/secret/12345.ts". The
// prefix segment's charset is restricted the same as username/password (word
// chars only) so the match can't slide left into a preceding host[:port]
// segment, which may itself contain dots/colons that would otherwise look
// like a valid prefix.
var streamLinkPattern = regexp.MustCompile(`/([A-Za-z0-9_-]+)/([A-Za-z0-9_-]+)/([A-Za-z0-9_-]+)/([^\s"']*)`)

// resolveStreamLinks finds every rewritten stream link in out and turns each
// back into the upstream URL it stands for, the same way handleStream does —
// asserting along the way that the link really does carry s's own credentials
// where the upstream's would have been.
func resolveStreamLinks(t *testing.T, s *feedService, out string) []string {
	t.Helper()
	matches := streamLinkPattern.FindAllStringSubmatch(out, -1)
	targets := make([]string, 0, len(matches))
	for _, m := range matches {
		assert.Equal(t, s.creds.Username, m[2])
		assert.Equal(t, s.creds.Password, m[3])
		rest, query, _ := strings.Cut(m[4], "?")
		// The link carries an escaped path; handleStream receives it decoded
		// by ServeMux's wildcard matching, so decode before rebuilding.
		decoded, err := url.PathUnescape(rest)
		require.NoError(t, err)
		targets = append(targets, s.streamTarget(m[1], decoded, query).String())
	}
	return targets
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
			_, err := buildService(FeedConfig{
				Name:        "f",
				UpstreamURL: u,
				Credentials: Credentials{Username: "a", Password: "b"},
				Timeout:     5 * time.Second,
			})
			assert.Error(t, err)
		})
	}
}

// TestNewFeedService_RejectsUnsafeCredentials covers credentials that a stream
// link's path would spell differently than the config does. A feed's own would
// then leave links no client can authenticate against; the upstream's would go
// unrecognized and stay in the link instead of being swapped out. Both are
// startup errors rather than unplayable channels or a quiet leak.
func TestNewFeedService_RejectsUnsafeCredentials(t *testing.T) {
	safe := Credentials{Username: "alice", Password: "secret"}
	tests := map[string]FeedConfig{
		"slash in username":      {Credentials: Credentials{Username: "ali/ce", Password: "secret"}},
		"space in password":      {Credentials: Credentials{Username: "alice", Password: "sec ret"}},
		"question in password":   {Credentials: Credentials{Username: "alice", Password: "sec?ret"}},
		"slash in upstream user": {Credentials: safe, UpstreamURL: "http://upstream.example.com/get.php?username=xtream%2Fuser"},
		"space in upstream pass": {Credentials: safe, UpstreamURL: "http://upstream.example.com/get.php?password=xtream+pass"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := tc
			cfg.Name = "f"
			if cfg.UpstreamURL == "" {
				cfg.UpstreamURL = "http://upstream.example.com/get.php"
			}
			_, err := buildService(cfg)
			assert.Error(t, err)
		})
	}
}

func TestNewFeedService_ValidUpstreamURL(t *testing.T) {
	s := newTestService(t, "http://example.com/get.php?type=m3u")
	assert.Equal(t, "example.com", s.target.Host)
}

func TestRewritePlaylist_PublicBaseURL(t *testing.T) {
	cfg := testFeedConfig("http://upstream.example.com/get.php")
	cfg.PublicBaseURL = "http://public.example.com:58034/"
	s := newServiceFor(t, cfg)

	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")
	body := []byte("#EXTM3U\n#EXTINF:10,\nsegment1.ts\n")

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "http://public.example.com:58034/stream/")
	assert.NotContains(t, out, "upstream.example.com") // raw upstream host never appears
	targets := resolveStreamLinks(t, s, out)
	require.Len(t, targets, 1)
	assert.Equal(t, "http://upstream.example.com/live/segment1.ts", targets[0])
}

func TestRewritePlaylist(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\nsegment1.ts\n#EXTINF:10,\nsegment2.ts?token=abc\n")

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "#EXTM3U")
	assert.NotContains(t, out, "upstream.example.com") // raw upstream host never appears

	targets := resolveStreamLinks(t, s, out)
	assert.Contains(t, targets, "http://upstream.example.com/live/segment1.ts")
	// A segment's own query survives the round trip — some upstreams put a
	// per-segment token there, and dropping it would make the segment 403.
	assert.Contains(t, targets, "http://upstream.example.com/live/segment2.ts?token=abc")
}

// TestRewritePlaylist_ForeignHostFoldsOntoUpstream pins the deliberate limit
// of rebuilding targets from config: a playlist URI pointing somewhere other
// than the configured upstream is still rewritten (its host must not leak),
// but resolves back against the upstream host, not the original one. Xtream
// feeds serve their segments from the upstream host, and pinning it is what
// makes a /stream request unable to reach any other address at all.
func TestRewritePlaylist_ForeignHostFoldsOntoUpstream(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\nhttp://other.com/segment2.ts\n")

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)
	assert.NotContains(t, out, "other.com")

	targets := resolveStreamLinks(t, s, out)
	require.Len(t, targets, 1)
	assert.Equal(t, "http://upstream.example.com/segment2.ts", targets[0])
}

func TestRewritePlaylist_ExcludeGroupPrefixes(t *testing.T) {
	cfg := testFeedConfig("http://upstream.example.com/get.php")
	cfg.ExcludeGroupPrefixes = []string{"movies:", "series:"}
	s := newServiceFor(t, cfg)

	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")
	body := []byte(`#EXTM3U
#EXTINF:-1 tvg-ID="" group-title="Sweden - Nationella",SE: SVT 1
http://upstream.example.com/live/svt1
#EXTINF:-1 tvg-ID="" group-title="Movies: Action",Some Movie
http://upstream.example.com/movie/12345.mkv
#EXTINF:-1 tvg-ID="" group-title="Series: Nordic Disney",Some Show S01 E01
http://upstream.example.com/series/67890.mkv
`)

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "SE: SVT 1")
	assert.Contains(t, out, "/stream/")
	assert.NotContains(t, out, "Some Movie")
	assert.NotContains(t, out, "movie/12345.mkv")
	assert.NotContains(t, out, "Some Show")
	assert.NotContains(t, out, "series/67890.mkv")
}

func TestRewritePlaylist_KeyAndMapTags(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte(`#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234
#EXT-X-MAP:URI="init.mp4"
segment1.ts
`)

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.Contains(t, out, "METHOD=AES-128")
	assert.Contains(t, out, "IV=0x1234")
	assert.NotContains(t, out, "upstream.example.com") // raw upstream host never appears
	targets := resolveStreamLinks(t, s, out)
	assert.Contains(t, targets, "http://upstream.example.com/live/key.bin")
	assert.Contains(t, targets, "http://upstream.example.com/live/init.mp4")
	assert.Contains(t, targets, "http://upstream.example.com/live/segment1.ts")
}

func TestRewriteAttrURI_SingleQuotes(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php")
	base, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	line := `#EXT-X-KEY:METHOD=AES-128,URI='key.bin'`
	out := s.rewriteAttrURI(line, base)
	assert.Contains(t, out, "URI='/stream/")
	assert.NotContains(t, out, "upstream.example.com") // raw upstream host never appears

	targets := resolveStreamLinks(t, s, out)
	require.Len(t, targets, 1)
	assert.Equal(t, "http://upstream.example.com/live/key.bin", targets[0])
}

// TestRewritePlaylist_HidesEmbeddedUpstreamCredentials covers the leak this
// whole rewrite scheme exists for: an Xtream upstream embeds its own
// username/password directly in the stream path (/live/<user>/<pass>/<id>.ts).
// writeStreamURI mimics that exact shape for the client-facing link, so the
// credentials sitting in those two segments must be this feed's own, never the
// upstream's — otherwise mimicking the shape would reintroduce the very leak
// it hides.
func TestRewritePlaylist_HidesEmbeddedUpstreamCredentials(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php?username=xtreamuser&password=xtreampass")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\nhttp://upstream.example.com/live/xtreamuser/xtreampass/12345.ts\n")

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.NotContains(t, out, "upstream.example.com") // upstream host
	assert.NotContains(t, out, "xtreamuser")           // upstream's own embedded username
	assert.NotContains(t, out, "xtreampass")           // upstream's own embedded password

	// The rewritten link still mimics the /<prefix>/<user>/<pass>/<file> shape...
	assert.Contains(t, out, "/live/alice/secret/12345.ts")
	// ...and resolves back to the real upstream URL, credentials and all.
	targets := resolveStreamLinks(t, s, out)
	require.Len(t, targets, 1)
	assert.Equal(t, "http://upstream.example.com/live/xtreamuser/xtreampass/12345.ts", targets[0])
}

// TestRewritePlaylist_HidesCredentialsOutsideXtreamShape covers a path that
// carries the upstream credentials somewhere other than the two segments an
// Xtream stream URL puts them in — Xtream's own HLS segment paths
// (/hlsr/<token>/<user>/<pass>/...) do exactly this. The link can't mimic that
// shape, so it falls back to carrying the whole path; the credentials in it
// still have to be swapped out, and swapped back on the way in.
func TestRewritePlaylist_HidesCredentialsOutsideXtreamShape(t *testing.T) {
	s := newTestService(t, "http://upstream.example.com/get.php?username=xtreamuser&password=xtreampass")
	srcURL, _ := url.Parse("http://upstream.example.com/live/index.m3u8")

	body := []byte("#EXTM3U\n#EXTINF:10,\n/hlsr/tok3n/xtreamuser/xtreampass/451/9/12345.ts\n")

	out, err := rewriteBody(s, body, srcURL)
	require.NoError(t, err)

	assert.NotContains(t, out, "xtreamuser")
	assert.NotContains(t, out, "xtreampass")
	assert.Contains(t, out, "/stream/alice/secret/hlsr/tok3n/alice/secret/451/9/12345.ts")

	targets := resolveStreamLinks(t, s, out)
	require.Len(t, targets, 1)
	assert.Equal(t, "http://upstream.example.com/hlsr/tok3n/xtreamuser/xtreampass/451/9/12345.ts", targets[0])
}

// TestStreamURIPathParts_RoundTrip checks that every path shape survives being
// split into a link and rebuilt, including the two that could otherwise be
// confused for each other: a real Xtream path and one that merely starts with
// the fallback prefix.
func TestStreamURIPathParts_RoundTrip(t *testing.T) {
	paths := []string{
		"/live/alice/secret/12345.ts",
		"/timeshift/alice/secret/60/2024-01-01:00-00/12345.ts",
		"/live/segment1.ts",
		"/hlsr/tok3n/alice/secret/451/9/12345.ts",
		"/stream/alice/secret/12345.ts", // collides with fallbackStreamPrefix
		"/",
		"",
	}
	creds := Credentials{Username: "alice", Password: "secret"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			prefix, rest := streamURIPathParts(path, creds)
			got := streamURIPath(prefix, creds, rest)
			assert.Equal(t, "/"+strings.Trim(path, "/"), got)
		})
	}
}

func TestServePlaylist_CachesAcrossRequests(t *testing.T) {
	upstream, upstreamHits := newCountingPlaylistUpstream(t)

	s := newTestService(t, upstream.URL+"/get.php")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		s.servePlaylist(ctx, w, http.Header{})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "#EXTM3U")
	}

	assert.Equal(t, int64(1), upstreamHits.Load(), "later requests should be served from cache, not re-fetched")
}

func TestServePlaylist_RefetchesAfterCacheExpires(t *testing.T) {
	upstream, upstreamHits := newCountingPlaylistUpstream(t)

	s := newTestService(t, upstream.URL+"/get.php")
	ctx := context.Background()

	w := httptest.NewRecorder()
	s.servePlaylist(ctx, w, http.Header{})
	assert.Equal(t, int64(1), upstreamHits.Load())

	// Force the cache to look stale rather than sleeping playlistCacheTTL.
	s.playlistCacheMu.Lock()
	s.playlistCachedAt = time.Now().Add(-playlistCacheTTL - time.Second)
	s.playlistCacheMu.Unlock()

	w = httptest.NewRecorder()
	s.servePlaylist(ctx, w, http.Header{})
	assert.Equal(t, int64(2), upstreamHits.Load(), "expired cache should trigger a fresh upstream fetch")
}
