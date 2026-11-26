package iptvproxy

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

// viewPlaylist is a feed's cached playlist as one view sees it: only the
// channels its rules allow, with the view's own credentials in every link, plus
// the set of channels it must not be able to play.
type viewPlaylist struct {
	body string
	// channels maps streamKey of every channel the playlist lists to whether
	// the view may play it.
	channels map[string]bool
}

// streamKey identifies a channel independent of how a client spells its link:
// rest must already be percent-decoded, and the extension is normalized away,
// because Xtream clients routinely ask for 1234.m3u8 where the playlist says
// 1234.ts. The Xtream live forms of one stream id (/live/u/p/ID.ts,
// /u/p/ID, /timeshift/u/p/<duration>/<start>/ID.ts) share one key, so a
// channel can't be reached through a sibling URL shape. isChannel reports
// whether the link has one of those forms.
func streamKey(prefix, rest string) (key string, isChannel bool) {
	segments := strings.Split(rest, "/")
	last := segments[len(segments)-1]
	stem := strings.TrimSuffix(last, path.Ext(last))
	switch {
	case (prefix == "live" || prefix == bareCredsStreamPrefix) && len(segments) == 1,
		prefix == "timeshift" && len(segments) == 3:
		return "live/" + stem, true
	}
	return prefix + "/" + strings.TrimSuffix(rest, path.Ext(rest)), false
}

// linkParts splits a rewritten link line, which always has the shape
// baseURLPlaceholder/<prefix>/<username>/<password>/<rest> (see
// writeStreamURI), into prefix and rest. ok is false for anything else.
func linkParts(line string) (prefix, rest string, ok bool) {
	after, found := strings.CutPrefix(strings.TrimSpace(line), baseURLPlaceholder+"/")
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(after, "/", 4)
	if len(parts) < 3 {
		return "", "", false
	}
	if len(parts) == 4 {
		rest, _, _ = strings.Cut(parts[3], "?")
	}
	return parts[0], rest, true
}

// swapLinkCredentials rewrites the feed credentials in every rewritten link of
// line (a bare URI line or a URI="..." attribute) to the view's. Only the
// credential slots of a placeholder link are touched, never free text.
func swapLinkCredentials(line string, from, to Credentials) string {
	var out strings.Builder
	for {
		before, after, found := strings.Cut(line, baseURLPlaceholder+"/")
		out.WriteString(before)
		if !found {
			return out.String()
		}
		out.WriteString(baseURLPlaceholder + "/")
		prefix, tail, ok := strings.Cut(after, "/")
		if !ok {
			out.WriteString(after)
			return out.String()
		}
		out.WriteString(prefix + "/")
		if rest, ok := strings.CutPrefix(tail, from.Username+"/"+from.Password+"/"); ok {
			out.WriteString(to.Username + "/" + to.Password + "/")
			line = rest
		} else {
			line = tail
		}
	}
}

// buildViewPlaylist filters body (a feed's rewritten playlist) down to what m
// allows. Entries are #EXTINF-led blocks ending at their URI line; everything
// outside a block (headers, #EXT-X tags) passes through untouched, as RFC 8216
// requires of line order.
func buildViewPlaylist(body string, m *curation.Matcher, feed, view Credentials) *viewPlaylist {
	var out strings.Builder
	out.Grow(len(body))
	writeLine := func(line string) {
		out.WriteString(swapLinkCredentials(line, feed, view))
		out.WriteByte('\n')
	}

	// allowed per channel key; a key shared by an allowed and a denied entry
	// (links differing only by query) can't be told apart by path, so allowed
	// wins.
	allowed := map[string]bool{}
	var pending []string
	var extinf string

	for raw := range strings.Lines(body) {
		line := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#EXTINF"):
			extinf = trimmed
			pending = append(pending[:0], line)
		case pending != nil && (trimmed == "" || strings.HasPrefix(trimmed, "#")):
			pending = append(pending, line)
		case pending != nil:
			ok := m.Allow(curation.ParseEntry(extinf, trimmed))
			if prefix, rest, isLink := linkParts(trimmed); isLink {
				if unescaped, err := url.PathUnescape(rest); err == nil {
					rest = unescaped
				}
				key, _ := streamKey(prefix, rest)
				allowed[key] = allowed[key] || ok
			}
			if ok {
				for _, p := range pending {
					writeLine(p)
				}
				writeLine(line)
			}
			pending = nil
		default:
			writeLine(line)
		}
	}

	return &viewPlaylist{body: strings.Clone(out.String()), channels: allowed}
}

// viewPlaylists memoizes buildViewPlaylist per view. The cached feed playlist
// is replaced only every playlistCacheTTL, so rebuilding on every poll or
// stream request would repeat a pass over tens of thousands of lines for
// nothing.
type viewPlaylists struct {
	mu    sync.Mutex
	items map[string]*viewPlaylistEntry
}

// viewPlaylistEntry has its own lock so one view's rebuild doesn't stall
// every other view's requests behind the map lock.
type viewPlaylistEntry struct {
	mu sync.Mutex
	// A new feed playlist, edited rules or new credentials invalidate the
	// entry. Comparing body costs a pointer check while the feed cache still
	// serves the same string.
	body    string
	matcher *curation.Matcher
	creds   Credentials
	value   *viewPlaylist
}

func (c *viewPlaylists) get(v *resolvedView, feed *feedService, body string) *viewPlaylist {
	c.mu.Lock()
	if c.items == nil {
		c.items = map[string]*viewPlaylistEntry{}
	}
	e := c.items[v.Name]
	if e == nil {
		e = &viewPlaylistEntry{}
		c.items[v.Name] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.value == nil || e.matcher != v.Matcher || e.creds != v.Credentials || e.body != body {
		e.value = buildViewPlaylist(body, v.Matcher, feed.creds, v.Credentials)
		e.body, e.matcher, e.creds = body, v.Matcher, v.Credentials
	}
	return e.value
}

// serveViewPlaylist serves the view's filtered copy of the feed's cached
// playlist.
func (p *Proxy) serveViewPlaylist(ctx context.Context, w http.ResponseWriter, header http.Header, baseURL string, f *feedService, v *resolvedView) {
	result, err := f.getPlaylist(ctx, header)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	vp := p.viewPlaylists.get(v, f, result.body)
	writePlaylistResponse(w, playlistFetchResult{body: vp.body, status: result.status}, baseURL)
}

// viewMayStream reports whether the view may play the stream link r asks for.
// A live channel the view's rules exclude, or the playlist doesn't list, is
// refused, so guessing or remembering its URL gets a view nowhere. Links that
// aren't live channel forms (HLS segments, sub-playlists) pass: they belong to
// a channel the view already reached.
func (p *Proxy) viewMayStream(r *http.Request, f *feedService, v *resolvedView, prefix string) bool {
	result, err := f.getPlaylist(r.Context(), r.Header)
	if err != nil {
		// Can't tell what is excluded; fail closed rather than open a gap
		// during an upstream outage.
		return false
	}
	vp := p.viewPlaylists.get(v, f, result.body)
	key, isChannel := streamKey(prefix, r.PathValue("rest"))
	if allowed, listed := vp.channels[key]; listed {
		return allowed
	}
	// A live channel the playlist doesn't list (say, one the feed's
	// exclude_group_prefixes dropped) isn't one the view's rules ever allowed.
	return !isChannel
}
