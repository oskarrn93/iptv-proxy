package iptvproxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleXtreamAPI_RequiresAuth mirrors TestHandlePlaylist_RewritesAndRequiresAuth's
// unauthorized case for the player_api.php route.
func TestHandleXtreamAPI_RequiresAuth(t *testing.T) {
	p := newTestProxy(t, "http://upstream.example.com/get.php")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHandleXtreamAPI_SwapsCredentialsAndRewritesResponse checks the full
// round trip: the upstream player_api.php sees the feed's real (upstream)
// credentials and whatever action/params the client sent, and the JSON it
// returns comes back to the client with user_info/server_info pointed at
// this proxy instead of the upstream.
func TestHandleXtreamAPI_SwapsCredentialsAndRewritesResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/player_api.php", r.URL.Path)
		assert.Equal(t, "xtreamuser", r.URL.Query().Get("username"))
		assert.Equal(t, "xtreampass", r.URL.Query().Get("password"))
		assert.Equal(t, "get_live_categories", r.URL.Query().Get("action"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"user_info": {"username": "xtreamuser", "password": "xtreampass", "auth": 1},
			"server_info": {"url": "upstream.example.com", "port": "8080", "https_port": "8443", "server_protocol": "http"}
		}`))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=alice&password=secret&action=get_live_categories", nil)
	req.Host = "foo.example.com"
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	assert.Contains(t, body, `"username":"alice"`)
	assert.Contains(t, body, `"password":"secret"`)
	assert.Contains(t, body, `"url":"foo.example.com"`)
	assert.Contains(t, body, `"port":"80"`)
	assert.Contains(t, body, `"https_port":"443"`)
	assert.NotContains(t, body, "xtreamuser")
	assert.NotContains(t, body, "xtreampass")
	assert.NotContains(t, body, "upstream.example.com")
}

// TestHandleXtreamAPI_RewritesDirectSourceURL checks that an absolute
// upstream stream URL embedded directly in a JSON field (as some Xtream
// upstreams do for VOD/series "direct_source") is rewritten into the same
// proxy link shape a playlist entry gets, rather than leaking the upstream.
func TestHandleXtreamAPI_RewritesDirectSourceURL(t *testing.T) {
	mux := http.NewServeMux()
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"stream_id": 1, "direct_source": "` + upstream.URL + `/movie/xtreamuser/xtreampass/1.mkv"}]`))
	})

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=alice&password=secret&action=get_vod_streams", nil)
	req.Host = "iptv.oskarrosen.io"
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "http://iptv.oskarrosen.io/movie/alice/secret/1.mkv")
	assert.NotContains(t, body, "xtreamuser")
	assert.NotContains(t, body, "xtreampass")
	assert.NotContains(t, body, upstream.URL)
}

// TestHandleXtreamAPI_PassesThroughNonJSON checks that a non-JSON upstream
// response (an error page, a misconfigured upstream) is forwarded as-is
// rather than dropped.
func TestHandleXtreamAPI_PassesThroughNonJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "not json", rec.Body.String())
}

// TestHandleXtreamAPI_ForwardsUpstreamErrorStatus mirrors the playlist/EPG
// routes' behavior for an upstream error status.
func TestHandleXtreamAPI_ForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")
	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestMux_XtreamAPIRoutesRegistered checks both player_api.php and its
// panel_api.php alias reach handleXtreamAPI through the real mux, not just
// when called directly.
func TestMux_XtreamAPIRoutesRegistered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php")
	mux := p.Mux()

	for _, path := range []string{"/player_api.php", "/panel_api.php"} {
		req := httptest.NewRequest(http.MethodGet, path+"?username=alice&password=secret", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "path %s", path)
		assert.JSONEq(t, `{"ok":true}`, rec.Body.String())
	}
}

func TestHandleXtreamAPI_BasicAuthStillSendsUpstreamCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "xtreamuser", r.URL.Query().Get("username"))
		assert.Equal(t, "xtreampass", r.URL.Query().Get("password"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php?action=get_live_categories", nil)
	req.SetBasicAuth("alice", "secret")
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandleXtreamAPI_OversizedBodyIsNotPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"direct_source":"http://upstream/live/xtreamuser/xtreampass/1.ts","pad":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxXtreamAPIBody))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/get.php?username=xtreamuser&password=xtreampass")

	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=alice&password=secret", nil)
	rec := httptest.NewRecorder()
	p.handleXtreamAPI(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.NotContains(t, rec.Body.String(), "xtreampass")
}
