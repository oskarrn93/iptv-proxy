package iptvproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHealthAggregator_InvalidListenAddr(t *testing.T) {
	_, err := NewHealthAggregator([]FeedConfig{{Name: "f", ListenAddr: "not-a-host-port"}})
	assert.Error(t, err)
}

func TestHealthAggregator_AllHealthy(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up2.Close()

	agg, err := NewHealthAggregator([]FeedConfig{
		{Name: "feed-1", ListenAddr: "127.0.0.1:" + portOf(t, up1.URL)},
		{Name: "feed-2", ListenAddr: "127.0.0.1:" + portOf(t, up2.URL)},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	agg.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestHealthAggregator_OneUnhealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	agg, err := NewHealthAggregator([]FeedConfig{
		{Name: "feed-ok", ListenAddr: "127.0.0.1:" + portOf(t, up.URL)},
		{Name: "feed-down", ListenAddr: "127.0.0.1:1"},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	agg.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "feed-down")
	assert.NotContains(t, rec.Body.String(), "feed-ok")
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Port()
}
