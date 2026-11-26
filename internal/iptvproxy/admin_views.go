package iptvproxy

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

//go:embed admin_views.html
var adminViewsPageHTML []byte

// maxPreviewRows bounds the channel table of the view editor's preview; a
// provider list runs to tens of thousands of entries.
const maxPreviewRows = 300

// requireViews answers 404 for a views route when views_file isn't set.
func (p *Proxy) requireViews(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.views == nil {
			http.Error(w, "views are disabled: set views_file in config.yaml", http.StatusNotFound)
			return
		}
		h(w, r)
	}
}

func (p *Proxy) handleAdminViewsPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminViewsPageHTML)
}

type adminViewRow struct {
	Name, Feed, Username, Summary string
}

func (p *Proxy) handleAdminViewList(w http.ResponseWriter, _ *http.Request) {
	views := p.views.List()
	rows := make([]adminViewRow, 0, len(views))
	for _, v := range views {
		rows = append(rows, adminViewRow{
			Name: v.Name, Feed: v.Feed, Username: v.Username,
			Summary: fmt.Sprintf("%d include, %d exclude, %d pinned, %d blocked",
				len(v.Rules.Include), len(v.Rules.Exclude), len(v.Rules.Pinned), len(v.Rules.Blocked)),
		})
	}
	renderAdminFragment(w, "view-list", rows)
}

// adminViewEditor is the data of the view editor form. Rules are edited as
// plain text, one entry per line.
type adminViewEditor struct {
	Original, Name, Feed, Username, Password string
	Include, Exclude, Pinned, Blocked        string
	Feeds                                    []string
	PlaylistURL                              string
}

func (p *Proxy) feedNames() []string {
	names := make([]string, 0, len(p.feeds))
	for _, f := range p.feeds {
		names = append(names, f.name)
	}
	return names
}

func (p *Proxy) handleAdminViewEditor(w http.ResponseWriter, r *http.Request) {
	ed := adminViewEditor{Feeds: p.feedNames()}
	if name := r.URL.Query().Get("name"); name != "" {
		v, err := p.views.Get(name)
		if err != nil {
			http.Error(w, "unknown view", http.StatusNotFound)
			return
		}
		ed.Original, ed.Name, ed.Feed = v.Name, v.Name, v.Feed
		ed.Username, ed.Password = v.Username, v.Password
		ed.Include, ed.Exclude = patternsText(v.Rules.Include), patternsText(v.Rules.Exclude)
		ed.Pinned, ed.Blocked = strings.Join(v.Rules.Pinned, "\n"), strings.Join(v.Rules.Blocked, "\n")
		q := url.Values{"username": {v.Username}, "password": {v.Password}}
		ed.PlaylistURL = requestBaseURL(r) + "/playlist.m3u8?" + q.Encode()
	}
	renderAdminFragment(w, "view-editor", ed)
}

// patternsText and parsePatterns convert rules to and from the editor's
// "field:regex" lines. A line with no recognized field prefix is a name regex.
func patternsText(ps []curation.Pattern) string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.Field + ":" + p.Regex
	}
	return strings.Join(lines, "\n")
}

func parsePatterns(text string) []curation.Pattern {
	var out []curation.Pattern
	for _, line := range splitLines(text) {
		field, regex := curation.FieldName, line
		if f, rest, ok := strings.Cut(line, ":"); ok {
			switch f {
			case curation.FieldName, curation.FieldGroup, curation.FieldTvgID:
				field, regex = f, rest
			}
		}
		out = append(out, curation.Pattern{Field: field, Regex: regex})
	}
	return out
}

// splitLines returns the trimmed, non-empty, de-duplicated lines of text.
func splitLines(text string) []string {
	var out []string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line != "" && !slices.Contains(out, line) {
			out = append(out, line)
		}
	}
	return out
}

func randomToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// rulesFromForm reads the editor's rule fields.
func rulesFromForm(r *http.Request) curation.Rules {
	return curation.Rules{
		Include: parsePatterns(r.PostFormValue("include")),
		Exclude: parsePatterns(r.PostFormValue("exclude")),
		Pinned:  splitLines(r.PostFormValue("pinned")),
		Blocked: splitLines(r.PostFormValue("blocked")),
	}
}

func (p *Proxy) handleAdminViewSave(w http.ResponseWriter, r *http.Request) {
	v := View{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Feed:        r.PostFormValue("feed"),
		Credentials: Credentials{Username: strings.TrimSpace(r.PostFormValue("username")), Password: strings.TrimSpace(r.PostFormValue("password"))},
		Rules:       rulesFromForm(r),
	}
	// A name is the view's identity in the URL and the store, so an existing
	// view can't be renamed.
	if original := r.PostFormValue("original"); original != "" && original != v.Name {
		http.Error(w, "a view cannot be renamed", http.StatusUnprocessableEntity)
		return
	}
	// Credentials are unguessable by default, like a feed's.
	for _, field := range []*string{&v.Username, &v.Password} {
		if *field == "" {
			tok, err := randomToken()
			if err != nil {
				http.Error(w, "could not generate credentials", http.StatusInternalServerError)
				return
			}
			*field = tok
		}
	}

	if err := p.views.Put(v); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errViewInvalid) {
			status = http.StatusUnprocessableEntity
		} else {
			loggerFromContext(r.Context(), p.logger).ErrorContext(r.Context(), "saving view failed", "view", v.Name, "error", err)
		}
		http.Error(w, err.Error(), status)
		return
	}
	loggerFromContext(r.Context(), p.logger).InfoContext(r.Context(), "view saved", "view", v.Name, "feed", v.Feed)
	w.Header().Set("HX-Trigger", "views-changed")
	fmt.Fprintf(w, "Saved %s. Reopen it from the list to see its credentials and playlist link.", v.Name)
}

func (p *Proxy) handleAdminViewDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := p.views.Delete(name); err != nil {
		if errors.Is(err, errViewNotFound) {
			http.Error(w, "unknown view", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	loggerFromContext(r.Context(), p.logger).InfoContext(r.Context(), "view deleted", "view", name)
	w.Header().Set("HX-Trigger", "views-changed")
	fmt.Fprintf(w, "Deleted %s.", name)
}

// toggleKey adds key to the named list of the editor form (or removes it if
// present), taking it off the opposite list, and re-renders both lists.
func (p *Proxy) handleAdminViewToggle(w http.ResponseWriter, r *http.Request) {
	list, key := r.PostFormValue("list"), r.PostFormValue("key")
	pinned, blocked := splitLines(r.PostFormValue("pinned")), splitLines(r.PostFormValue("blocked"))
	var target, other *[]string
	switch list {
	case "pinned":
		target, other = &pinned, &blocked
	case "blocked":
		target, other = &blocked, &pinned
	default:
		http.Error(w, "list must be pinned or blocked", http.StatusBadRequest)
		return
	}
	if slices.Contains(*target, key) {
		*target = slices.DeleteFunc(*target, func(k string) bool { return k == key })
	} else {
		*target = append(*target, key)
		*other = slices.DeleteFunc(*other, func(k string) bool { return k == key })
	}
	w.Header().Set("HX-Trigger", "rules-changed")
	renderAdminFragment(w, "view-lists", adminViewEditor{Pinned: strings.Join(pinned, "\n"), Blocked: strings.Join(blocked, "\n")})
}

type adminPreviewRow struct {
	Name, Group, Key   string
	Allowed            bool
	Pinned, Blocked    bool
	PinVals, BlockVals string
}

type adminPreview struct {
	Error     string
	Total     int
	Allowed   int
	Orphans   []string
	Rows      []adminPreviewRow
	Truncated bool
}

// channelEntries parses a feed's rewritten playlist into the entries rules are
// evaluated against.
func channelEntries(body string) []curation.Entry {
	var entries []curation.Entry
	extinf := ""
	for raw := range strings.Lines(body) {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			extinf = line
		case line == "" || strings.HasPrefix(line, "#"):
		case extinf != "":
			entries = append(entries, curation.ParseEntry(extinf, line))
			extinf = ""
		}
	}
	return entries
}

func jsonVals(list, key string) string {
	b, _ := json.Marshal(map[string]string{"list": list, "key": key})
	return string(b)
}

func (p *Proxy) handleAdminViewPreview(w http.ResponseWriter, r *http.Request) {
	feed := p.feedByName(r.PostFormValue("feed"))
	if feed == nil {
		renderAdminFragment(w, "view-preview", adminPreview{Error: "choose a feed"})
		return
	}
	rules := rulesFromForm(r)
	m, err := curation.Compile(rules)
	if err != nil {
		renderAdminFragment(w, "view-preview", adminPreview{Error: err.Error()})
		return
	}
	result, err := feed.getPlaylist(r.Context(), nil)
	if err != nil {
		renderAdminFragment(w, "view-preview", adminPreview{Error: "could not load the feed's playlist"})
		return
	}

	entries := channelEntries(result.body)
	q := strings.ToLower(strings.TrimSpace(r.PostFormValue("q")))
	prev := adminPreview{Total: len(entries), Orphans: m.Orphans(entries)}
	for _, e := range entries {
		allowed := m.Allow(e)
		if allowed {
			prev.Allowed++
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Name), q) {
			continue
		}
		if len(prev.Rows) >= maxPreviewRows {
			prev.Truncated = true
			continue
		}
		key := e.Key()
		prev.Rows = append(prev.Rows, adminPreviewRow{
			Name: e.Name, Group: e.Group, Key: key, Allowed: allowed,
			Pinned: slices.Contains(rules.Pinned, key), Blocked: slices.Contains(rules.Blocked, key),
			PinVals: jsonVals("pinned", key), BlockVals: jsonVals("blocked", key),
		})
	}
	renderAdminFragment(w, "view-preview", prev)
}
