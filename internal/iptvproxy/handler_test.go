package iptvproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newProxyWith builds a Proxy over the given feeds, holding the Config and
// NewProxy plumbing that every proxy test would otherwise repeat.
func newProxyWith(t *testing.T, metricsEnabled bool, feeds ...FeedConfig) *Proxy {
	t.Helper()
	p, err := NewProxy(&Config{ListenAddr: ":0", Feeds: feeds}, testLogger(), metricsEnabled)
	require.NoError(t, err)
	return p
}

func newTestProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	return newProxyWith(t, true, testFeedConfig(upstream))
}

func TestNewProxy_InvalidUpstreamURL(t *testing.T) {
	_, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds: []FeedConfig{{
			Name:        "f",
			UpstreamURL: "not a url",
			Credentials: Credentials{Username: "a", Password: "b"},
		}},
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
	p := newProxyWith(t, true,
		FeedConfig{Name: "feed-1", UpstreamURL: "http://upstream1.example.com/get.php", Credentials: Credentials{Username: "alice", Password: "secret1"}},
		FeedConfig{Name: "feed-2", UpstreamURL: "http://upstream2.example.com/get.php", Credentials: Credentials{Username: "bob", Password: "secret2"}},
	)

	req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=bob&password=secret2", nil)
	rec := httptest.NewRecorder()
	f := p.authenticate(rec, req)
	require.NotNil(t, f)
	assert.Equal(t, "feed-2", f.name)
}

func TestHandlePlaylist_RewritesAndRequiresAuth(t *testing.T) {
	// The upstream's own response embeds Xtream-style credentials in the
	// segment path, same as writeStreamURI's own client-facing link will —
	// this is what must never come through unrewritten.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n/live/xtreamuser/xtreampass/segment1.ts\n"))
	}))
	defer upstream.Close()

	// The upstream's credentials have to be the ones the config declares:
	// swapping them out of a rewritten link is only possible for credentials
	// the proxy knows about, which for an Xtream feed is exactly the pair its
	// get.php URL carries.
	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

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
		assert.Contains(t, body, "/live/") // link shape still mimics an Xtream stream URL
		assert.NotContains(t, body, upstream.URL)
		assert.NotContains(t, body, "xtreamuser") // upstream's own embedded username never leaks
		assert.NotContains(t, body, "xtreampass") // upstream's own embedded password never leaks
	})
}

// streamRequest builds a stream request for the link
// /<prefix>/<username>/<password>/<rest>, setting the path values directly
// (via (*http.Request).SetPathValue, Go 1.22+) rather than routing through
// p.Mux() first, since these tests call p.handleStream directly — ServeMux is
// what would normally populate PathValue from the matched
// /{prefix}/{username}/{password}/{rest...} pattern.
func streamRequest(prefix, username, password, rest string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/"+prefix+"/"+username+"/"+password+"/"+rest, nil)
	req.SetPathValue("prefix", prefix)
	req.SetPathValue("username", username)
	req.SetPathValue("password", password)
	req.SetPathValue("rest", rest)
	return req
}

func TestHandleStream_RejectsBadCredentials(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	t.Run("no credentials in path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/stream", nil)
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("unknown username", func(t *testing.T) {
		req := streamRequest("live", "mallory", "secret", "0")
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("known username, wrong password", func(t *testing.T) {
		req := streamRequest("live", "alice", "wrong", "0")
		rec := httptest.NewRecorder()
		p.handleStream(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

// TestHandleStream_ProxiesSegment walks a link end to end: the path carries
// the feed's own credentials where the upstream's stood, and handleStream has
// to put the upstream's back — reaching /live/xtreamuser/xtreampass/segment.ts
// on the configured upstream, not the path the client actually requested.
func TestHandleStream_ProxiesSegment(t *testing.T) {
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

	rec := httptest.NewRecorder()
	p.handleStream(rec, streamRequest("live", "alice", "secret", "segment.ts"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte{0x47, 0x01, 0x02, 0x03}, rec.Body.Bytes())
}

// TestHandleStream_CannotReachAnotherHost is the reason there is no separate
// SSRF check on the /stream route: the target host comes from config, so
// nothing a client puts in the path can steer the fetch off the configured
// upstream, however it is spelled.
func TestHandleStream_CannotReachAnotherHost(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		assert.Equal(t, "/127.0.0.1:9/secret", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	rec := httptest.NewRecorder()
	p.handleStream(rec, streamRequest("stream", "alice", "secret", "127.0.0.1:9/secret"))

	assert.True(t, reached, "the configured upstream is the only host a stream request can reach")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleStream_ExposesActiveSessionsByKind checks that a completed
// /stream request is reflected in the activeSessions gauge under the right
// "kind" label — live for a channel prefix, vod for a movie/series one — and
// that the gauge is back at 0 once the request has finished, since the
// increment/decrement brackets the whole handler (see handleStream).
func TestHandleStream_ExposesActiveSessionsByKind(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write([]byte{0x47})
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	p.handleStream(httptest.NewRecorder(), streamRequest("live", "alice", "secret", "1.ts"))
	p.handleStream(httptest.NewRecorder(), streamRequest("movie", "alice", "secret", "1.mkv"))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `iptv_proxy_active_sessions{feed="test",kind="live"} 0`)
	assert.Contains(t, body, `iptv_proxy_active_sessions{feed="test",kind="vod"} 0`)
}

// TestHandlePlaylist_BaseURLPerRequestHost is the end-to-end case this
// whole mechanism exists for: the same feed, same cached playlist fetch,
// serving distinct absolute /stream links to a request that reached the
// proxy on the LAN vs. one that came in through a reverse proxy's
// X-Forwarded-Host — with no public_base_url configured anywhere. The
// upstream hit counter pins that the second request is served from
// playlistCache (see servePlaylist), not a second fetch+rewrite — proving
// the host substitution itself happens on every response, cached or not,
// rather than being baked in once at rewrite time.
func TestHandlePlaylist_BaseURLPerRequestHost(t *testing.T) {
	upstream, upstreamHits := newCountingPlaylistUpstream(t)

	p := newTestProxy(t, upstream.URL+"/get.php")

	lanReq := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
	lanReq.Host = "192.168.1.12:58033"
	lanRec := httptest.NewRecorder()
	p.handlePlaylist(lanRec, lanReq)
	require.Equal(t, http.StatusOK, lanRec.Code)
	assert.Contains(t, lanRec.Body.String(), "http://192.168.1.12:58033/stream/")

	extReq := httptest.NewRequest(http.MethodGet, "/playlist.m3u8?username=alice&password=secret", nil)
	extReq.Host = "internal-host:58033"
	extReq.Header.Set("X-Forwarded-Host", "iptv.example.com")
	extReq.Header.Set("X-Forwarded-Proto", "https")
	extRec := httptest.NewRecorder()
	p.handlePlaylist(extRec, extReq)
	require.Equal(t, http.StatusOK, extRec.Code)
	assert.Contains(t, extRec.Body.String(), "https://iptv.example.com/stream/")
	assert.NotContains(t, extRec.Body.String(), "internal-host")

	assert.Equal(t, int64(1), upstreamHits.Load(), "second request must be served from cache, not refetched, to prove the host swap happens per response")
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

// TestHandleEPG_RequiresAuthAndFetchesXMLTV checks handleEPG rejects an
// unauthenticated request the same way handlePlaylist does, and that an
// authenticated one reaches the upstream's /xmltv.php with the upstream's own
// (not the client-facing) credentials — the same swap streamTarget performs.
func TestHandleEPG_RequiresAuthAndFetchesXMLTV(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/xmltv.php", r.URL.Path)
		assert.Equal(t, "xtreamuser", r.URL.Query().Get("username"))
		assert.Equal(t, "xtreampass", r.URL.Query().Get("password"))
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<tv></tv>"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	t.Run("unauthorized without creds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/epg", nil)
		rec := httptest.NewRecorder()
		p.handleEPG(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("authorized request fetches upstream xmltv.php", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/epg?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		p.handleEPG(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "<tv></tv>", rec.Body.String())
		assert.Equal(t, "application/xml", rec.Header().Get("Content-Type"))
	})
}

// TestHandleEPG_Caches pins that a second request within epgCacheTTL is
// served from cache rather than triggering a second upstream fetch, mirroring
// TestHandlePlaylist_BaseURLPerRequestHost's cache assertion for the playlist
// route.
func TestHandleEPG_Caches(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<tv></tv>"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/epg?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		p.handleEPG(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	assert.Equal(t, int64(1), hits.Load(), "second request must be served from cache, not refetched")
}

func TestHandleEPG_ForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")
	req := httptest.NewRequest(http.MethodGet, "/epg?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	p.handleEPG(rec, req)

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
	p := newProxyWith(t, false, testFeedConfig("http://upstream.example.com/get.php"))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
