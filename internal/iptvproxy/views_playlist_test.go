package iptvproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

const viewTestPlaylist = `#EXTM3U
#EXTINF:-1 tvg-id="news" group-title="News",BBC News
http://up.example/live/u1/p1/101.ts
#EXTINF:-1 group-title="Sports",Sky Sports
#EXTVLCOPT:network-caching=1000
http://up.example/live/u1/p1/102.ts
#EXTINF:-1 group-title="Sports",ESPN
http://up.example/live/u1/p1/103.ts
`

func newViewProxy(t *testing.T, rules curation.Rules) (*Proxy, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get.php" {
			io.WriteString(w, viewTestPlaylist)
			return
		}
		io.WriteString(w, "stream-bytes")
	}))
	t.Cleanup(upstream.Close)

	feed := testFeedConfig(upstream.URL + "/get.php?username=u1&password=p1")
	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds:      []FeedConfig{feed},
		ViewsFile:  filepath.Join(t.TempDir(), "views.json"),
	}, testLogger(), false)
	require.NoError(t, err)
	require.NoError(t, p.views.Put(View{
		Name: "sports", Feed: "test", Credentials: Credentials{Username: "viewer", Password: "vpw"}, Rules: rules,
	}))
	return p, upstream
}

func get(p *Proxy, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestView_PlaylistFiltersAndRewritesCredentials(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{Include: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Sports"}}})

	rec := get(p, "/playlist.m3u8?username=viewer&password=vpw")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "#EXTM3U")
	assert.Contains(t, body, "Sky Sports")
	assert.Contains(t, body, "#EXTVLCOPT:network-caching=1000", "tags inside an allowed entry are kept")
	assert.Contains(t, body, "ESPN")
	assert.NotContains(t, body, "BBC News")
	assert.NotContains(t, body, "101.ts", "an excluded entry's link goes with it")
	assert.Contains(t, body, "/live/viewer/vpw/102.ts")

	for _, leak := range []string{"alice", "secret", "u1", "p1", "up.example", "\x00"} {
		assert.NotContains(t, body, leak)
	}
}

func TestView_PlaylistReMatchesWhenUpstreamChanges(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{Include: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Sports"}}})
	first := get(p, "/playlist.m3u8?username=viewer&password=vpw").Body.String()
	assert.NotContains(t, first, "Eurosport")

	f := p.feedByName("test")
	res, _ := f.playlistCache.lastStored()
	res.body += "#EXTINF:-1 group-title=\"Sports\",Eurosport\n" + baseURLPlaceholder + "/live/alice/secret/104.ts\n"
	f.playlistCache.store(res)

	assert.Contains(t, get(p, "/playlist.m3u8?username=viewer&password=vpw").Body.String(), "Eurosport")
}

func TestView_StreamGate(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{Include: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Sports"}}})
	get(p, "/playlist.m3u8?username=viewer&password=vpw") // warm the feed cache

	assert.Equal(t, http.StatusOK, get(p, "/live/viewer/vpw/102.ts").Code)
	assert.Equal(t, http.StatusNotFound, get(p, "/live/viewer/vpw/101.ts").Code)
	assert.Equal(t, http.StatusNotFound, get(p, "/live/viewer/vpw/101.m3u8").Code, "another extension must not bypass the gate")
	assert.Equal(t, http.StatusNotFound, get(p, "/live/viewer/vpw/%31%30%31.ts").Code, "percent-encoding must not bypass the gate")
	assert.Equal(t, http.StatusOK, get(p, "/hls/viewer/vpw/segment-9.ts").Code, "links that are not listed channels pass")

	assert.Equal(t, http.StatusOK, get(p, "/live/alice/secret/101.ts").Code, "the feed itself is unfiltered")
	assert.Equal(t, http.StatusUnauthorized, get(p, "/live/viewer/wrong/102.ts").Code)
}

func TestView_FeedPlaylistStaysUnfiltered(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{Include: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Sports"}}})
	body := get(p, "/playlist.m3u8?username=alice&password=secret").Body.String()
	assert.Contains(t, body, "BBC News")
	assert.Contains(t, body, "/live/alice/secret/101.ts")
}

func TestView_WrongPasswordRejected(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	assert.Equal(t, http.StatusUnauthorized, get(p, "/playlist.m3u8?username=viewer&password=nope").Code)
}

func TestSwapLinkCredentials(t *testing.T) {
	from, to := Credentials{Username: "a", Password: "b"}, Credentials{Username: "x", Password: "y"}
	p := baseURLPlaceholder
	assert.Equal(t, p+"/live/x/y/1.ts", swapLinkCredentials(p+"/live/a/b/1.ts", from, to))
	assert.Equal(t, `#EXT-X-KEY:URI="`+p+`/stream/x/y/k",IV=0`, swapLinkCredentials(`#EXT-X-KEY:URI="`+p+`/stream/a/b/k",IV=0`, from, to))
	assert.Equal(t, "#EXTINF:-1,/a/b/ show", swapLinkCredentials("#EXTINF:-1,/a/b/ show", from, to), "free text is never touched")
}
