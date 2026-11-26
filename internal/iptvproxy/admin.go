package iptvproxy

import (
	"cmp"
	"embed"
	"html/template"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

// adminPageHTML is the static admin UI (see admin.html): pick a feed, pick
// one of its channels, choose a TTL, and mint a share link — via the same
// POST /admin/shares endpoint any other admin client uses. Every /admin/*
// route, including this page, has no auth check of its own — see
// handleCreateShare for why (adminOnly gates the whole path instead).
//
//go:embed admin.html
var adminPageHTML []byte

// adminStatic holds the admin page's own CSS/JS and the vendored htmx files
// (pinned version — see static/README.md), served locally rather than fetched
// from a CDN so the page works with no outbound internet access, matching
// every other route in this LAN-only proxy.
//
//go:embed static/*.js static/*.css
var adminStatic embed.FS

// adminFragmentsHTML holds the markup the admin API returns for htmx to swap.
//
//go:embed admin_fragments.html
var adminFragmentsHTML string

var adminFragments = template.Must(template.New("").Parse(adminFragmentsHTML))

// renderAdminFragment writes one template from admin_fragments.html. Headers
// are already sent by the time execution can fail, so there is no status left
// to report an error with.
func renderAdminFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminFragments.ExecuteTemplate(w, name, data)
}

// adminOption is one <option> in an admin page feed or channel list.
type adminOption struct {
	Value string
	Label string
}

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

// handleAdminStatic serves the files in adminStatic. No-store for the same
// reason as handleAdminPage.
func (p *Proxy) handleAdminStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	body, err := adminStatic.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleAdminFeeds lists configured feed names as <option> elements for the
// admin page's feed <select> — HTML rather than JSON, since the page is an
// htmx page that swaps markup fragments directly rather than parsing JSON
// itself.
func (p *Proxy) handleAdminFeeds(w http.ResponseWriter, r *http.Request) {
	options := make([]adminOption, 0, len(p.feeds))
	for _, f := range p.feeds {
		options = append(options, adminOption{Value: f.name, Label: f.name})
	}
	renderAdminFragment(w, "options", options)
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

	baseURL := requestBaseURL(r)
	entries := parseChannelEntries(result.body)
	options := make([]adminOption, 0, len(entries))
	for _, ch := range entries {
		options = append(options, adminOption{Value: applyBaseURL(ch.URL, baseURL), Label: ch.Name})
	}
	renderAdminFragment(w, "options", options)
}

// handleListShares renders every currently-active share as an HTML table row
// (see shareStore.list), each with a revoke button — the admin page polls
// this after creating a share and after revoking one, rather than tracking
// share state of its own in JS.
func (p *Proxy) handleListShares(w http.ResponseWriter, r *http.Request) {
	baseURL := requestBaseURL(r)
	shares := p.shares.list()
	rows := make([]adminShareRow, 0, len(shares))
	for _, sh := range shares {
		rows = append(rows, adminShareRow{
			Token:   sh.Token,
			Channel: cmp.Or(sh.ChannelName, "(unnamed channel)"),
			Feed:    sh.Feed,
			Kind:    string(sh.Kind),
			Expires: sh.ExpiresAt.Format("15:04:05 MST"),
			URL:     baseURL + "/share/" + sh.Token,
		})
	}
	renderAdminFragment(w, "shares", rows)
}

// adminShareRow is one row of the admin page's active shares table.
type adminShareRow struct {
	Token   string
	Channel string
	Feed    string
	Kind    string
	Expires string
	URL     string
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

	pendingName := ""
	havePending := false
	for raw := range strings.Lines(body) {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			pendingName = curation.DisplayName(line)
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
