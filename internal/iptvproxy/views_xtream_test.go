package iptvproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

func newXtreamViewProxy(t *testing.T, rules curation.Rules) *Proxy {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("username") != "u1" || q.Get("password") != "p1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch q.Get("action") {
		case "get_live_categories":
			w.Write([]byte(`[{"category_id":"1","category_name":"News"},{"category_id":"2","category_name":"Sports"},{"category_id":"3","category_name":"Adult"}]`))
		case "get_live_streams":
			w.Write([]byte(`[
				{"name":"BBC News","stream_id":101,"category_id":"1","epg_channel_id":"bbc"},
				{"name":"Sky Sports","stream_id":102,"category_id":"2"},
				{"name":"Hidden","stream_id":103,"category_id":"3"}]`))
		case "get_vod_streams":
			w.Write([]byte(`[{"name":"Movie","direct_source":"` + "http://" + r.Host + `/movie/u1/p1/9.mkv"}]`))
		default:
			w.Write([]byte(`{"user_info":{"username":"u1","password":"p1","auth":1},"server_info":{"url":"` + r.Host + `"}}`))
		}
	}))
	t.Cleanup(upstream.Close)

	p, err := NewProxy(&Config{
		ListenAddr: ":0",
		Feeds:      []FeedConfig{testFeedConfig(upstream.URL + "/get.php?username=u1&password=p1")},
		ViewsFile:  filepath.Join(t.TempDir(), "views.json"),
	}, testLogger(), false)
	require.NoError(t, err)
	require.NoError(t, p.views.Put(View{
		Name: "v", Feed: "test", Credentials: Credentials{Username: "viewer", Password: "vpw"}, Rules: rules,
	}))
	return p
}

func apiNames(t *testing.T, body []byte, key string) []string {
	t.Helper()
	var items []map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &items), string(body))
	var out []string
	for _, it := range items {
		out = append(out, it[key].(string))
	}
	return out
}

func TestViewXtream_LiveStreamsFilteredByGroup(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{Exclude: []curation.Pattern{{Field: curation.FieldGroup, Regex: "^Adult$"}}})
	rec := get(p, "/player_api.php?username=viewer&password=vpw&action=get_live_streams")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"BBC News", "Sky Sports"}, apiNames(t, rec.Body.Bytes(), "name"))
}

func TestViewXtream_CategoriesDropEmptyOnes(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{Include: []curation.Pattern{{Field: curation.FieldName, Regex: "Sports"}}})
	rec := get(p, "/player_api.php?username=viewer&password=vpw&action=get_live_categories")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"Sports"}, apiNames(t, rec.Body.Bytes(), "category_name"))
}

func TestViewXtream_LoginUsesViewCredentials(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{})
	rec := get(p, "/player_api.php?username=viewer&password=vpw")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `"username":"viewer"`)
	assert.Contains(t, body, `"password":"vpw"`)
	for _, leak := range []string{"u1", "p1", "alice", "secret"} {
		assert.NotContains(t, body, leak)
	}
}

func TestViewXtream_OtherActionsPassThroughWithViewLinks(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{})
	rec := get(p, "/player_api.php?username=viewer&password=vpw&action=get_vod_streams")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "/movie/viewer/vpw/9.mkv")
	assert.NotContains(t, rec.Body.String(), "u1")
}

func TestViewXtream_FeedStaysUnfiltered(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{Exclude: []curation.Pattern{{Field: curation.FieldGroup, Regex: "^Adult$"}}})
	rec := get(p, "/player_api.php?username=alice&password=secret&action=get_live_streams")
	assert.Len(t, apiNames(t, rec.Body.Bytes(), "name"), 3)
}

func TestViewXtream_FilterFailureIsNotServedUnfiltered(t *testing.T) {
	p := newXtreamViewProxy(t, curation.Rules{Exclude: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Adult"}}})
	// Wrong upstream credentials make the category lookup fail with a 403.
	p.feedByName("test").upstreamCreds = Credentials{Username: "bad", Password: "bad"}
	rec := get(p, "/player_api.php?username=viewer&password=vpw&action=get_live_streams")
	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.NotContains(t, rec.Body.String(), "Hidden")
}
