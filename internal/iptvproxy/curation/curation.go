// Package curation decides which channels of a playlist a view may see.
// It is pure (no I/O, no HTTP) so rules can be re-evaluated against every
// freshly fetched playlist: a rule set is stored, never its results.
package curation

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Entry is one channel of a playlist.
type Entry struct {
	Name     string
	Group    string
	TvgID    string
	StreamID string
}

// Key is the stable identity pins and blocks refer to. It deliberately is not
// the URL, which changes whenever the upstream rotates hosts or tokens.
func (e Entry) Key() string {
	switch {
	case e.TvgID != "":
		return "tvg:" + e.TvgID
	case e.StreamID != "":
		return "id:" + e.StreamID
	default:
		return "name:" + strings.ToLower(strings.TrimSpace(e.Name))
	}
}

var (
	attrPattern = func(attr string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)` + attr + `="([^"]*)"`)
	}
	groupTitlePattern = attrPattern("group-title")
	tvgIDPattern      = attrPattern("tvg-id")
)

// DisplayName returns the channel name from a #EXTINF line: the text after its
// last comma, per the M3U EXTINF format (duration,title — any tvg-* attributes
// precede the comma, never follow it).
func DisplayName(extinf string) string {
	if idx := strings.LastIndex(extinf, ","); idx != -1 && idx+1 < len(extinf) {
		return strings.TrimSpace(extinf[idx+1:])
	}
	return extinf
}

// ParseEntry builds an Entry from a #EXTINF line and the URI line after it.
func ParseEntry(extinf, uri string) Entry {
	e := Entry{Name: DisplayName(extinf)}
	if m := groupTitlePattern.FindStringSubmatch(extinf); m != nil {
		e.Group = m[1]
	}
	if m := tvgIDPattern.FindStringSubmatch(extinf); m != nil {
		e.TvgID = m[1]
	}
	// Xtream stream links end in /<numeric stream_id>.<ext>.
	base := path.Base(strings.SplitN(uri, "?", 2)[0])
	id := strings.TrimSuffix(base, path.Ext(base))
	if id != "" && strings.Trim(id, "0123456789") == "" {
		e.StreamID = id
	}
	return e
}

const (
	FieldName  = "name"
	FieldGroup = "group"
	FieldTvgID = "tvg_id"
)

// Pattern is a regular expression applied to one field of an Entry.
type Pattern struct {
	Field string `json:"field"`
	Regex string `json:"regex"`
}

// Rules is the stored, user-edited selection.
type Rules struct {
	Include []Pattern `json:"include,omitempty"`
	Exclude []Pattern `json:"exclude,omitempty"`
	Pinned  []string  `json:"pinned,omitempty"`
	Blocked []string  `json:"blocked,omitempty"`
}

type compiledPattern struct {
	field string
	re    *regexp.Regexp
}

// Matcher is a compiled Rules, safe for concurrent use.
type Matcher struct {
	include, exclude []compiledPattern
	pinned, blocked  map[string]struct{}
}

// UsesGroup reports whether any pattern looks at the group, so a caller that
// has to look group names up can skip the lookup when no rule needs it.
func (m *Matcher) UsesGroup() bool {
	for _, ps := range [][]compiledPattern{m.include, m.exclude} {
		for _, p := range ps {
			if p.field == FieldGroup {
				return true
			}
		}
	}
	return false
}

func compile(ps []Pattern) ([]compiledPattern, error) {
	out := make([]compiledPattern, 0, len(ps))
	for _, p := range ps {
		switch p.Field {
		case FieldName, FieldGroup, FieldTvgID:
		default:
			return nil, fmt.Errorf("unknown field %q", p.Field)
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", p.Regex, err)
		}
		out = append(out, compiledPattern{p.Field, re})
	}
	return out, nil
}

func set(keys []string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// Compile validates and compiles rules.
func Compile(r Rules) (*Matcher, error) {
	inc, err := compile(r.Include)
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	exc, err := compile(r.Exclude)
	if err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	return &Matcher{inc, exc, set(r.Pinned), set(r.Blocked)}, nil
}

func (c compiledPattern) matches(e Entry) bool {
	switch c.field {
	case FieldGroup:
		return c.re.MatchString(e.Group)
	case FieldTvgID:
		return c.re.MatchString(e.TvgID)
	default:
		return c.re.MatchString(e.Name)
	}
}

func anyMatch(ps []compiledPattern, e Entry) bool {
	for _, p := range ps {
		if p.matches(e) {
			return true
		}
	}
	return false
}

// Allow applies, in order: blocked, pinned, exclude, include. With no include
// patterns everything not otherwise excluded is allowed.
func (m *Matcher) Allow(e Entry) bool {
	key := e.Key()
	if _, ok := m.blocked[key]; ok {
		return false
	}
	if _, ok := m.pinned[key]; ok {
		return true
	}
	if anyMatch(m.exclude, e) {
		return false
	}
	return len(m.include) == 0 || anyMatch(m.include, e)
}

// Orphans returns pinned and blocked keys that no entry in the current
// playlist carries any more, so the UI can flag rules the upstream outgrew.
func (m *Matcher) Orphans(entries []Entry) []string {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.Key()] = struct{}{}
	}
	var out []string
	for _, s := range []map[string]struct{}{m.pinned, m.blocked} {
		for k := range s {
			if _, ok := seen[k]; !ok {
				out = append(out, k)
			}
		}
	}
	return out
}
