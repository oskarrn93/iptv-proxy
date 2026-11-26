package iptvproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxXtreamAPIBody caps how much of a player_api.php JSON response this proxy
// will buffer in memory to rewrite. A large provider's VOD/series listing can
// run to several MB; this is a safety ceiling, not a typical size.
const maxXtreamAPIBody = 32 << 20

// xtreamAPIPath is the upstream path every player_api.php-shaped request is
// proxied to, regardless of which alias (player_api.php, panel_api.php) the
// client itself reached this proxy on — real Xtream servers only ever
// implement the one endpoint.
const xtreamAPIPath = "/player_api.php"

// xtreamAPITarget rebuilds the upstream player_api.php URL for a client's own
// API request: this feed's configured upstream scheme/host (never anything a
// client supplies, same reasoning as streamTarget/epgTarget), with the
// client's query string carried over verbatim except its own credentials
// swapped back for the upstream's — the reverse of what a rewritten stream
// link does, applied to a query string alone since player_api.php never
// receives a path beyond the endpoint itself.
func (s *feedService) xtreamAPITarget(rawQuery string) *url.URL {
	_, query := s.restoreCredentials.apply("", rawQuery)
	// A client that authenticated with Basic Auth has no credentials in its
	// query for the swap to restore, and the upstream only reads them there.
	values, _ := url.ParseQuery(query)
	if !values.Has("username") && !values.Has("password") && s.upstreamCreds != (Credentials{}) {
		values.Set("username", s.upstreamCreds.Username)
		values.Set("password", s.upstreamCreds.Password)
		query = values.Encode()
	}
	return &url.URL{Scheme: s.target.Scheme, Host: s.target.Host, Path: xtreamAPIPath, RawQuery: query}
}

// serveXtreamAPI proxies one player_api.php call: fetches the client's own
// query (action, category_id, vod_id, ...) against the feed's real upstream
// with its credentials swapped in, then rewrites the JSON response so nothing
// that would let a client bypass this proxy — the upstream's own host, or the
// credentials it embeds — reaches the client. Unlike the playlist/EPG routes
// this is not cached: a request here carries arbitrary query params (a
// per-VOD/series lookup, a category filter), so there is no single stable
// target to coalesce concurrent requests onto the way servePlaylist/serveEPG
// do for their one fixed upstream URL.
func (s *feedService) serveXtreamAPI(reqCtx context.Context, w http.ResponseWriter, reqHeader http.Header, rawQuery, baseURL string, filter xtreamFilter) {
	logger := loggerFromContext(reqCtx, s.logger)

	status, body, pfErr := s.fetchXtreamAPI(reqCtx, reqHeader, rawQuery)
	if pfErr != nil {
		writeUpstreamError(w, pfErr)
		return
	}

	out, err := s.rewriteXtreamAPIBody(reqCtx, body, baseURL, filter)
	if err != nil {
		logger.ErrorContext(reqCtx, "xtream api filter failed", "error", redactErr(err))
		writeUpstreamError(w, &playlistFetchError{status: http.StatusBadGateway, msg: "filter failed"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// xtreamFilter narrows a decoded player_api.php response before it is
// rewritten; a view uses it to drop channels its rules exclude. An error means
// the response could not be filtered and must not be served.
type xtreamFilter func(parsed interface{}) (interface{}, error)

// fetchXtreamAPI performs one upstream player_api.php call for the client's
// query and returns its status and raw body.
func (s *feedService) fetchXtreamAPI(reqCtx context.Context, reqHeader http.Header, rawQuery string) (int, []byte, *playlistFetchError) {
	logger := loggerFromContext(reqCtx, s.logger)
	target := s.xtreamAPITarget(rawQuery)

	tctx, cancel := context.WithTimeout(reqCtx, s.timeout)
	defer cancel()

	resp, err := s.repo.fetchWithRetry(tctx, target.String(), reqHeader)
	if err != nil {
		return 0, nil, s.upstreamFailure(tctx, err)
	}
	defer resp.Body.Close()

	if pfErr := s.upstreamStatusFailure(tctx, target, resp); pfErr != nil {
		return 0, nil, pfErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXtreamAPIBody+1))
	if err != nil {
		logger.ErrorContext(reqCtx, "xtream api read failed", "error", redactErr(err))
		return 0, nil, &playlistFetchError{status: http.StatusBadGateway, msg: "read failed"}
	}

	// Truncated JSON would fail to parse and take the unrewritten passthrough
	// in rewriteXtreamAPIBody, handing the client whatever upstream URLs it
	// holds.
	if len(body) > maxXtreamAPIBody {
		logger.ErrorContext(reqCtx, "xtream api response too large to rewrite", "limit", maxXtreamAPIBody)
		return 0, nil, &playlistFetchError{status: http.StatusBadGateway, msg: "upstream response too large"}
	}
	return resp.StatusCode, body, nil
}

// rewriteXtreamAPIBody rewrites a player_api.php JSON response so it never
// exposes the upstream: a nested "user_info" object's credentials become this
// feed's own (see rewriteUserInfo), a nested "server_info" object's host/ports
// point back at this proxy (see rewriteServerInfo), and any absolute URL
// string pointing at the upstream host — a VOD/series "direct_source" or
// similar field some upstreams populate with a ready-to-play URL — is
// rewritten through the same /stream link shape a playlist entry gets (see
// rewriteXtreamURLString). A body that isn't valid JSON (an upstream error
// page, an empty response) is passed through unrewritten rather than dropped,
// so the client still sees whatever the upstream actually said.
func (s *feedService) rewriteXtreamAPIBody(ctx context.Context, body []byte, baseURL string, filter xtreamFilter) ([]byte, error) {
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A view's filter can't run on a body it can't read, and an unfiltered
		// passthrough would show it everything.
		if filter != nil {
			return nil, err
		}
		loggerFromContext(ctx, s.logger).WarnContext(ctx, "xtream api response not JSON, passing through unrewritten", "error", err)
		return body, nil
	}
	if filter != nil {
		var err error
		if parsed, err = filter(parsed); err != nil {
			return nil, err
		}
	}

	rewritten, err := json.Marshal(s.rewriteXtreamValue(parsed, baseURL))
	if err != nil {
		loggerFromContext(ctx, s.logger).ErrorContext(ctx, "xtream api response re-encode failed, passing through unrewritten", "error", err)
		if filter != nil {
			return nil, err
		}
		return body, nil
	}
	return rewritten, nil
}

// rewriteXtreamValue walks a decoded player_api.php JSON value (object,
// array, or scalar), applying rewriteUserInfo/rewriteServerInfo to any nested
// "user_info"/"server_info" object it finds by key, and rewriteXtreamURLString
// to every string leaf — cheaply a no-op for the vast majority of strings
// (channel names, dates, an image URL on some other CDN host) since that only
// ever rewrites a string that parses as an absolute URL on this feed's own
// upstream host. Recognizing "user_info"/"server_info" by key (rather than by
// which fields the object happens to have) matches how every Xtream
// implementation names them, and avoids misfiring on some other object that
// merely happens to have a "url" or "username" field of its own.
func (s *feedService) rewriteXtreamValue(v interface{}, baseURL string) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, sub := range val {
			switch k {
			case "user_info":
				if m, ok := sub.(map[string]interface{}); ok {
					s.rewriteUserInfo(m)
					continue
				}
			case "server_info":
				if m, ok := sub.(map[string]interface{}); ok {
					s.rewriteServerInfo(m, baseURL)
					continue
				}
			}
			val[k] = s.rewriteXtreamValue(sub, baseURL)
		}
		return val
	case []interface{}:
		for i, item := range val {
			val[i] = s.rewriteXtreamValue(item, baseURL)
		}
		return val
	case string:
		return s.rewriteXtreamURLString(val, baseURL)
	default:
		return val
	}
}

// rewriteUserInfo swaps an Xtream "user_info" object's username/password —
// present verbatim as the upstream's own credentials, since this proxy
// requested player_api.php with those (see xtreamAPITarget) — for this feed's
// own, so a client reading its own login back out of the response (as the
// reference Xtream client does, to build subsequent stream URLs) sees the
// credentials it must keep using against this proxy, not the upstream's. Uses
// s.hideCredentials — the same upstream-to-feed swap streamTarget's rewrite
// path relies on — rather than comparing fields by hand.
func (s *feedService) rewriteUserInfo(m map[string]interface{}) {
	if u, ok := m["username"].(string); ok {
		m["username"] = s.hideCredentials.value(u)
	}
	if p, ok := m["password"].(string); ok {
		m["password"] = s.hideCredentials.value(p)
	}
}

// rewriteServerInfo points an Xtream "server_info" object's host/ports at
// this proxy instead of the upstream, so a client that builds its own stream
// URLs from server_info (rather than using the links this proxy already
// rewrites elsewhere) still reaches back through this proxy. baseURL is the
// scheme://host[:port] the requesting client itself reached this proxy
// through (see requestBaseURL) — the same source of truth applyBaseURL uses
// for /stream links — so a client on a nonstandard port (e.g. reaching
// iptv-proxy-go directly on its listen_addr rather than through Traefik) gets
// that port back rather than an assumed 80/443. Each field is only touched if
// the upstream's own response already carried it, so an upstream with a
// nonstandard server_info shape doesn't gain fields it never had.
func (s *feedService) rewriteServerInfo(m map[string]interface{}, baseURL string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return
	}
	httpPort, httpsPort := "80", "443"
	if p := u.Port(); p != "" {
		if u.Scheme == "https" {
			httpsPort = p
		} else {
			httpPort = p
		}
	}
	if _, ok := m["url"]; ok {
		m["url"] = u.Hostname()
	}
	if _, ok := m["port"]; ok {
		m["port"] = httpPort
	}
	if _, ok := m["https_port"]; ok {
		m["https_port"] = httpsPort
	}
	if _, ok := m["server_protocol"]; ok {
		m["server_protocol"] = u.Scheme
	}
}

// rewriteXtreamURLString rewrites raw if it is an absolute URL on this feed's
// own upstream host into the same /<prefix>/<username>/<password>/<file>
// proxy link shape a rewritten playlist entry gets (see writeStreamURI) —
// covering fields like a VOD/series entry's "direct_source" that some Xtream
// upstreams populate with a ready-to-play URL instead of leaving the client to
// build one from user_info/server_info. Anything else (a name, a date, an
// image URL on some other CDN host) is returned unchanged. The scheme prefix
// check avoids running url.Parse over every string leaf in a response —
// channel names, dates, ids — that can never be an absolute URL in the first
// place; a large VOD/series listing can have thousands of such leaves.
func (s *feedService) rewriteXtreamURLString(raw, baseURL string) string {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Host, s.target.Host) {
		return raw
	}
	var out strings.Builder
	s.writeStreamURI(&out, u)
	return applyBaseURL(out.String(), baseURL)
}
