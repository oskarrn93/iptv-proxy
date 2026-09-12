package iptvproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAdminStaticRoutesNoStore guards against a stale browser tab silently
// running old admin.html/JS across a deploy — see admin.go's handleAdminPage
// comment for the incident this came from (a fixed combobox bug that looked
// unfixed because the browser was still serving a cached copy).
func TestAdminStaticRoutesNoStore(t *testing.T) {
	p := &Proxy{}
	handlers := map[string]http.HandlerFunc{
		"admin page":  p.handleAdminPage,
		"htmx.js":     p.handleAdminHTMX,
		"json-enc.js": p.handleAdminJSONEnc,
	}

	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.NotEmpty(t, rec.Body.Bytes())
		})
	}
}

func TestHandleAdminPageServesEmbeddedHTML(t *testing.T) {
	p := &Proxy{}
	rec := httptest.NewRecorder()
	p.handleAdminPage(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), `<script src="/admin/static/htmx.js"></script>`)
	assert.Contains(t, rec.Body.String(), `<script src="/admin/static/json-enc.js"></script>`)
}
