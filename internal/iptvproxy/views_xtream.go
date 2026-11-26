package iptvproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

// forView returns s speaking as one of its views: links and logins it writes
// carry the view's credentials, and the view's credentials in a request are
// swapped for the upstream's. Everything else, the upstream and the metrics
// included, is shared with the feed.
func (s *feedService) forView(c Credentials) *feedService {
	vs := *s
	hide := credentialSwap{from: s.upstreamCreds, to: c}
	vs.creds = c
	vs.hideCredentials = hide
	vs.restoreCredentials = hide.reversed()
	return &vs
}

// serveViewXtreamAPI serves player_api.php to a view. Live channel and
// category listings are filtered by the view's rules; every other action
// (VOD, series, EPG lookups, login) passes through unfiltered, with the view's
// own credentials in place of the upstream's.
func (p *Proxy) serveViewXtreamAPI(ctx context.Context, w http.ResponseWriter, header http.Header, rawQuery, baseURL string, f *feedService, v *resolvedView) {
	// The proxy and the upstream must agree on which action runs: a repeated
	// parameter could show the filter one and the upstream another.
	if query, _ := url.ParseQuery(rawQuery); len(query["action"]) > 1 {
		http.Error(w, "repeated action parameter", http.StatusBadRequest)
		return
	}
	vs := f.forView(v.Credentials)
	vs.serveXtreamAPI(ctx, w, header, rawQuery, baseURL, vs.liveFilter(ctx, header, rawQuery, v.Matcher))
}

// liveFilter returns the filter for the request's action, or nil when the
// action isn't one a view restricts.
func (s *feedService) liveFilter(ctx context.Context, header http.Header, rawQuery string, m *curation.Matcher) xtreamFilter {
	query, _ := url.ParseQuery(rawQuery)
	switch query.Get("action") {
	case "get_live_streams":
		return func(parsed interface{}) (interface{}, error) {
			streams, err := asObjects(parsed)
			if err != nil {
				return nil, err
			}
			groups, err := s.liveCategoryNames(ctx, header, rawQuery, m)
			if err != nil {
				return nil, err
			}
			return filterLiveStreams(streams, groups, m), nil
		}
	case "get_live_categories":
		return func(parsed interface{}) (interface{}, error) {
			categories, err := asObjects(parsed)
			if err != nil {
				return nil, err
			}
			all, err := s.fetchXtreamList(ctx, header, rawQuery, "get_live_streams")
			if err != nil {
				return nil, err
			}
			groups := categoryNames(categories)
			kept := map[string]bool{}
			for _, st := range filterLiveStreams(all, groups, m) {
				kept[scalar(st.(map[string]interface{})["category_id"])] = true
			}
			out := make([]interface{}, 0, len(categories))
			for _, c := range categories {
				if kept[scalar(c["category_id"])] {
					out = append(out, c)
				}
			}
			return out, nil
		}
	}
	return nil
}

// liveCategoryNames maps category_id to name for group-based rules. It costs
// an extra upstream call, so it is made only when a rule looks at groups.
func (s *feedService) liveCategoryNames(ctx context.Context, header http.Header, rawQuery string, m *curation.Matcher) (map[string]string, error) {
	if !m.UsesGroup() {
		return nil, nil
	}
	categories, err := s.fetchXtreamList(ctx, header, rawQuery, "get_live_categories")
	if err != nil {
		return nil, err
	}
	return categoryNames(categories), nil
}

func categoryNames(categories []map[string]interface{}) map[string]string {
	names := make(map[string]string, len(categories))
	for _, c := range categories {
		names[scalar(c["category_id"])], _ = c["category_name"].(string)
	}
	return names
}

// fetchXtreamList fetches an unfiltered list action for the same login as the
// client's own request.
func (s *feedService) fetchXtreamList(ctx context.Context, header http.Header, rawQuery, action string) ([]map[string]interface{}, error) {
	query, _ := url.ParseQuery(rawQuery)
	query.Set("action", action)
	query.Del("category_id")
	status, body, pfErr := s.fetchXtreamAPI(ctx, header, query.Encode())
	if pfErr != nil {
		return nil, pfErr
	}
	if status >= 400 {
		return nil, fmt.Errorf("upstream returned %d for %s", status, action)
	}
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return asObjects(parsed)
}

// asObjects reads a decoded JSON array of objects. An upstream that answers an
// empty list with `false` or `null` counts as empty.
func asObjects(parsed interface{}) ([]map[string]interface{}, error) {
	switch val := parsed.(type) {
	case nil, bool:
		return nil, nil
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(val))
		for _, item := range val {
			m, ok := item.(map[string]interface{})
			if !ok {
				return nil, errors.New("unexpected list item in xtream response")
			}
			out = append(out, m)
		}
		return out, nil
	}
	return nil, errors.New("unexpected xtream response shape")
}

// scalar renders a JSON id, which providers send as either a string or a
// number, as a string.
func scalar(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	}
	return ""
}

// filterLiveStreams returns the allowed streams as []interface{}, the shape
// rewriteXtreamValue walks.
func filterLiveStreams(streams []map[string]interface{}, groups map[string]string, m *curation.Matcher) []interface{} {
	out := make([]interface{}, 0, len(streams))
	for _, st := range streams {
		name, _ := st["name"].(string)
		tvg, _ := st["epg_channel_id"].(string)
		entry := curation.Entry{
			Name:     name,
			Group:    groups[scalar(st["category_id"])],
			TvgID:    tvg,
			StreamID: scalar(st["stream_id"]),
		}
		if m.Allow(entry) {
			out = append(out, st)
		}
	}
	return out
}
