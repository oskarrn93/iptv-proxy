package iptvproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

func postForm(h http.HandlerFunc, target string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestParsePatterns(t *testing.T) {
	got := parsePatterns("group:^Sports\n\n  tvg_id:bbc \nplain regex\ngroup:^Sports\nhttp:odd")
	assert.Equal(t, []curation.Pattern{
		{Field: "group", Regex: "^Sports"},
		{Field: "tvg_id", Regex: "bbc"},
		{Field: "name", Regex: "plain regex"},
		{Field: "name", Regex: "http:odd"},
	}, got)
	assert.Equal(t, "group:^Sports\ntvg_id:bbc", patternsText(got[:2]))
}

func TestAdminViews_SaveCreatesViewWithGeneratedCredentials(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	rec := postForm(p.handleAdminViewSave, "/admin/views", url.Values{
		"name": {"kids"}, "feed": {"test"}, "include": {"group:Kids"}, "pinned": {"tvg:a"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "views-changed", rec.Header().Get("HX-Trigger"))

	v, err := p.views.Get("kids")
	require.NoError(t, err)
	assert.Len(t, v.Username, 24)
	assert.Len(t, v.Password, 24)
	assert.Equal(t, []curation.Pattern{{Field: "group", Regex: "Kids"}}, v.Rules.Include)
	assert.Equal(t, []string{"tvg:a"}, v.Rules.Pinned)
}

func TestAdminViews_SaveRejectsInvalid(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	rec := postForm(p.handleAdminViewSave, "/admin/views", url.Values{
		"name": {"bad"}, "feed": {"test"}, "include": {"name:("},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid regex")

	rec = postForm(p.handleAdminViewSave, "/admin/views", url.Values{
		"original": {"sports"}, "name": {"renamed"}, "feed": {"test"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAdminViews_EditorShowsCredentialsAndPlaylistLink(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	rec := httptest.NewRecorder()
	p.handleAdminViewEditor(rec, httptest.NewRequest(http.MethodGet, "/admin/views/editor?name=sports", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `value="viewer"`)
	assert.Contains(t, rec.Body.String(), "/playlist.m3u8?password=vpw&amp;username=viewer")

	rec = httptest.NewRecorder()
	p.handleAdminViewEditor(rec, httptest.NewRequest(http.MethodGet, "/admin/views/editor?name=nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminViews_Delete(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	req := httptest.NewRequest(http.MethodDelete, "/admin/views/sports", nil)
	req.SetPathValue("name", "sports")
	rec := httptest.NewRecorder()
	p.handleAdminViewDelete(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, p.views.List())

	rec = httptest.NewRecorder()
	p.handleAdminViewDelete(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminViews_PreviewCountsAndFlagsOrphans(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	get(p, "/playlist.m3u8?username=alice&password=secret") // fills the feed cache; preview reads it

	rec := postForm(p.handleAdminViewPreview, "/admin/views/preview", url.Values{
		"feed": {"test"}, "include": {"group:Sports"}, "pinned": {"tvg:news\ntvg:gone"},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<strong>3</strong> of 3", "the pinned news channel joins the two sports ones")
	assert.Contains(t, body, "tvg:gone")
	assert.Contains(t, body, "BBC News")

	rec = postForm(p.handleAdminViewPreview, "/admin/views/preview", url.Values{"feed": {"test"}, "q": {"espn"}})
	assert.NotContains(t, rec.Body.String(), "BBC News")
	assert.Contains(t, rec.Body.String(), "ESPN")

	rec = postForm(p.handleAdminViewPreview, "/admin/views/preview", url.Values{"feed": {"test"}, "include": {"name:("}})
	assert.Contains(t, rec.Body.String(), "invalid regex")
}

func TestAdminViews_ToggleMovesKeyBetweenLists(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	rec := postForm(p.handleAdminViewToggle, "/admin/views/toggle", url.Values{
		"list": {"pinned"}, "key": {"tvg:a"}, "blocked": {"tvg:a\ntvg:b"},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "rules-changed", rec.Header().Get("HX-Trigger"))
	assert.Contains(t, rec.Body.String(), ">tvg:a</textarea>", "pinned now holds it")
	assert.Contains(t, rec.Body.String(), ">tvg:b</textarea>", "and blocked no longer does")

	rec = postForm(p.handleAdminViewToggle, "/admin/views/toggle", url.Values{"list": {"pinned"}, "key": {"tvg:a"}, "pinned": {"tvg:a"}})
	assert.NotContains(t, rec.Body.String(), "tvg:a")

	assert.Equal(t, http.StatusBadRequest, postForm(p.handleAdminViewToggle, "/", url.Values{"list": {"x"}}).Code)
}

func TestAdminViews_RoutesAreAdminOnlyAndNeedViewsFile(t *testing.T) {
	p, _ := newViewProxy(t, curation.Rules{})
	// No trusted proxy or admin_auth is configured, so adminOnly refuses.
	assert.Equal(t, http.StatusForbidden, get(p, "/admin/views").Code)
	assert.Equal(t, http.StatusForbidden, get(p, "/admin/views/list").Code)

	disabled := newTestProxy(t, "http://upstream.example.com/get.php")
	rec := httptest.NewRecorder()
	disabled.requireViews(disabled.handleAdminViewList)(rec, httptest.NewRequest(http.MethodGet, "/admin/views/list", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
