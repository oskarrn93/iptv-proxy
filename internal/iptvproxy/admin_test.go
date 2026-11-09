package iptvproxy

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminStaticRoutesNoStore guards against a stale browser tab silently
// running old admin.html/JS across a deploy — see admin.go's handleAdminPage
// comment for the incident this came from (a fixed combobox bug that looked
// unfixed because the browser was still serving a cached copy).
func TestAdminStaticRoutesNoStore(t *testing.T) {
	p := &Proxy{}

	t.Run("admin page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		p.handleAdminPage(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.NotEmpty(t, rec.Body.Bytes())
	})

	for file, contentType := range map[string]string{
		"htmx.js":     "javascript",
		"json-enc.js": "javascript",
		"admin.js":    "javascript",
		"admin.css":   "text/css",
	} {
		t.Run(file, func(t *testing.T) {
			rec := serveAdminStatic(p, file)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.Contains(t, rec.Header().Get("Content-Type"), contentType)
			assert.NotEmpty(t, rec.Body.Bytes())
		})
	}
}

func serveAdminStatic(p *Proxy, file string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/static/"+file, nil)
	req.SetPathValue("file", file)
	rec := httptest.NewRecorder()
	p.handleAdminStatic(rec, req)
	return rec
}

func TestHandleAdminStatic_UnknownFileIsNotFound(t *testing.T) {
	for _, file := range []string{"missing.js", "README.md", "../admin.html"} {
		assert.Equal(t, http.StatusNotFound, serveAdminStatic(&Proxy{}, file).Code, file)
	}
}

func TestHandleAdminPageServesEmbeddedHTML(t *testing.T) {
	p := &Proxy{}
	rec := httptest.NewRecorder()
	p.handleAdminPage(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), `<script src="/admin/static/htmx.js"></script>`)
	assert.Contains(t, rec.Body.String(), `<script src="/admin/static/json-enc.js"></script>`)
	assert.Contains(t, rec.Body.String(), `<script src="/admin/static/admin.js"></script>`)
	assert.Contains(t, rec.Body.String(), `href="/admin/static/admin.css"`)
}

func TestHandleAdminFeeds_ListsFeeds(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	rec := httptest.NewRecorder()

	p.handleAdminFeeds(rec, httptest.NewRequest(http.MethodGet, "/admin/feeds", nil))

	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, `<option value="test">test</option>`, rec.Body.String())
}

func TestHandleAdminChannels_EscapesNamesAndResolvesLinks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,<img src=x onerror=alert(1)>\nhttp://ignored/live/u/p/1.ts\n"))
	}))
	defer upstream.Close()
	p := newTestProxy(t, upstream.URL+"/get.php?username=u&password=p")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/channels?feed=test", nil)
	req.Host = "iptv.example.com"

	p.handleAdminChannels(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<img")
	assert.Contains(t, rec.Body.String(), "&lt;img")
	assert.Contains(t, rec.Body.String(), "http://iptv.example.com/")
	assert.NotContains(t, rec.Body.String(), baseURLPlaceholder)
}

func TestHandleListShares_Empty(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	rec := httptest.NewRecorder()

	p.handleListShares(rec, httptest.NewRequest(http.MethodGet, "/admin/shares", nil))

	assert.Contains(t, rec.Body.String(), "no active shares")
}

func TestHandleListShares_RendersRevokableRow(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")
	target, _ := url.Parse("http://upstream.example.com/live/u/p/1.ts")
	token, _ := p.shares.create(p.feeds[0], target, sessionKindLive, time.Hour, "<b>News</b>")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/shares", nil)
	req.Host = "iptv.example.com"

	p.handleListShares(rec, req)

	body := html.UnescapeString(rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "<b>News")
	assert.Contains(t, body, `hx-delete="/admin/shares/`+token+`"`)
	assert.Contains(t, body, `hx-target="#share-`+token+`"`)
	assert.True(t, strings.Contains(body, `href="http://iptv.example.com/share/`+token+`"`), body)
}
