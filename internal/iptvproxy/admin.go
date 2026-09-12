package iptvproxy

import (
	"bufio"
	"cmp"
	_ "embed"
	"fmt"
	"html"
	"net/http"
	"strings"
)

// adminPageHTML is the static admin UI (see admin.html): pick a feed, pick
// one of its channels, choose a TTL, and mint a share link — via the same
// POST /admin/shares endpoint any other admin client uses. Every /admin/*
// route, including this page, has no auth check of its own — see
// handleCreateShare for why (Traefik/tinyauth gates the whole path instead).
//
//go:embed admin.html
var adminPageHTML []byte

// adminHTMXJS and adminJSONEncJS are vendored copies of htmx and its
// json-enc extension (pinned version — see static/README.md), served
// locally rather than fetched from a CDN so the admin page works with no
// outbound internet access, matching every other route in this LAN-only
// proxy. admin.html's <script src> tags must point at whatever path these
// are registered under (see handler.go) — keep both in sync when bumping
// the vendored version. admin_ui_test's Playwright suite serves these same
// files (via server.js), not a separate copy, so there's nothing else to
// keep in sync for tests.
//
//go:embed static/htmx.js
var adminHTMXJS []byte

//go:embed static/json-enc.js
var adminJSONEncJS []byte

// handleAdminPage serves the static admin page. No-store since this page's
// JS has already shipped fixes for bugs that only reproduced from a stale
// cached copy (a browser tab open across a deploy keeps running the old
// admin.html/JS in memory) — the page changes rarely enough that always
// refetching costs nothing.
func (p *Proxy) handleAdminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminPageHTML)
}

// handleAdminHTMX and handleAdminJSONEnc serve the vendored JS backing
// admin.html's <script src> tags — see adminHTMXJS/adminJSONEncJS. Also
// no-store, for the same reason as handleAdminPage.
func (p *Proxy) handleAdminHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminHTMXJS)
}

func (p *Proxy) handleAdminJSONEnc(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminJSONEncJS)
}

// handleAdminFeeds lists configured feed names as <option> elements for the
// admin page's feed <select> — HTML rather than JSON, since the page is an
// htmx page that swaps markup fragments directly rather than parsing JSON
// itself.
func (p *Proxy) handleAdminFeeds(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, f := range p.feeds {
		fmt.Fprintf(w, "<option value=\"%s\">%s</option>", html.EscapeString(f.name), html.EscapeString(f.name))
	}
}

// handleAdminChannels lists the requested feed's channels as <option>
// elements for the admin page's channel <select>, each option's value being
// the channel's own rewritten /stream link — exactly what POST /admin/shares
// expects as channel_url, so the admin page never has to know this proxy's
// internal prefix/credential layout.
func (p *Proxy) handleAdminChannels(w http.ResponseWriter, r *http.Request) {
	feed := p.feedByName(r.URL.Query().Get("feed"))
	if feed == nil {
		http.Error(w, "unknown feed", http.StatusNotFound)
		return
	}

	result, err := feed.getPlaylist(r.Context(), nil)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, ch := range parseChannelEntries(applyBaseURL(result.body, requestBaseURL(r))) {
		fmt.Fprintf(w, "<option value=\"%s\">%s</option>", html.EscapeString(ch.URL), html.EscapeString(ch.Name))
	}
}

// handleListShares renders every currently-active share as an HTML table row
// (see shareStore.list), each with a revoke button — the admin page polls
// this after creating a share and after revoking one, rather than tracking
// share state of its own in JS.
func (p *Proxy) handleListShares(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	shares := p.shares.list()
	if len(shares) == 0 {
		fmt.Fprint(w, "<tr><td colspan=\"6\"><em>no active shares</em></td></tr>")
		return
	}
	baseURL := requestBaseURL(r)
	for _, sh := range shares {
		token := html.EscapeString(sh.Token)
		shareURL := baseURL + "/share/" + sh.Token
		fmt.Fprintf(w, "<tr id=\"share-%s\">"+
			"<td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><a href=\"%s\" target=\"_blank\" rel=\"noopener\">watch</a></td>"+
			"<td><button class=\"small\" hx-delete=\"/admin/shares/%s\" hx-target=\"#share-%s\" hx-swap=\"outerHTML\">revoke</button></td>"+
			"</tr>",
			token,
			html.EscapeString(cmp.Or(sh.ChannelName, "(unnamed channel)")),
			html.EscapeString(sh.Feed),
			html.EscapeString(string(sh.Kind)),
			html.EscapeString(sh.ExpiresAt.Format("15:04:05 MST")),
			html.EscapeString(shareURL),
			token,
			token,
		)
	}
}

// channelEntry is one parsed #EXTINF/URI pair from a feed's own rewritten
// playlist.
type channelEntry struct {
	Name string
	URL  string
}

// parseChannelEntries pulls channel name + URL pairs out of an already
// rewritten playlist body, the same #EXTINF/URI pairing any IPTV client
// reads — feedService.rewritePlaylist changes a URI's host/credentials, never
// which #EXTINF it follows.
func parseChannelEntries(body string) []channelEntry {
	var entries []channelEntry
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	pendingName := ""
	havePending := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			pendingName = extinfDisplayName(line)
			havePending = true
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			if havePending {
				entries = append(entries, channelEntry{Name: pendingName, URL: line})
				havePending = false
			}
		}
	}
	return entries
}

// extinfDisplayName returns the channel name from a #EXTINF line: the text
// after its last comma, per the M3U EXTINF format (duration,title — any
// tvg-* attributes precede the comma, never follow it).
func extinfDisplayName(line string) string {
	if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
		return strings.TrimSpace(line[idx+1:])
	}
	return line
}
