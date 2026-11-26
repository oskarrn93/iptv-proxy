package curation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEntry(t *testing.T) {
	e := ParseEntry(`#EXTINF:-1 tvg-id="bbc1.uk" group-title="UK | News",BBC One`, "http://h/live/u/p/1234.ts?x=1")
	assert.Equal(t, Entry{Name: "BBC One", Group: "UK | News", TvgID: "bbc1.uk", StreamID: "1234"}, e)

	e = ParseEntry(`#EXTINF:-1,Plain`, "http://h/stream/index.m3u8")
	assert.Equal(t, Entry{Name: "Plain"}, e)
}

func TestEntryKey(t *testing.T) {
	assert.Equal(t, "tvg:a", Entry{TvgID: "a", StreamID: "1", Name: "x"}.Key())
	assert.Equal(t, "id:1", Entry{StreamID: "1", Name: "x"}.Key())
	assert.Equal(t, "name:bbc one", Entry{Name: "  BBC One "}.Key())
}

func TestAllow(t *testing.T) {
	news := Entry{Name: "BBC News", Group: "UK", TvgID: "bbcn"}
	sport := Entry{Name: "Sky Sports", Group: "UK"}
	adult := Entry{Name: "XXX", Group: "Adult"}

	tests := []struct {
		name  string
		rules Rules
		want  [3]bool
	}{
		{"empty allows all", Rules{}, [3]bool{true, true, true}},
		{"include by group", Rules{Include: []Pattern{{FieldGroup, "^UK$"}}}, [3]bool{true, true, false}},
		{"exclude wins over include", Rules{
			Include: []Pattern{{FieldGroup, "UK"}},
			Exclude: []Pattern{{FieldName, "(?i)sports"}},
		}, [3]bool{true, false, false}},
		{"pin beats exclude and missing include", Rules{
			Include: []Pattern{{FieldGroup, "UK"}},
			Exclude: []Pattern{{FieldName, "Sports"}},
			Pinned:  []string{sport.Key(), adult.Key()},
		}, [3]bool{true, true, true}},
		{"block beats pin and include", Rules{
			Include: []Pattern{{FieldGroup, "UK"}},
			Pinned:  []string{news.Key()},
			Blocked: []string{news.Key()},
		}, [3]bool{false, true, false}},
		{"tvg_id field", Rules{Include: []Pattern{{FieldTvgID, "^bbc"}}}, [3]bool{true, false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Compile(tt.rules)
			require.NoError(t, err)
			got := [3]bool{m.Allow(news), m.Allow(sport), m.Allow(adult)}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCompileErrors(t *testing.T) {
	_, err := Compile(Rules{Include: []Pattern{{FieldName, "("}}})
	assert.ErrorContains(t, err, "invalid regex")
	_, err = Compile(Rules{Exclude: []Pattern{{"bogus", "x"}}})
	assert.ErrorContains(t, err, "unknown field")
}

// A view re-matches when the upstream playlist changes: the same stored rules
// give a different result for a new playlist.
func TestRematchAfterPlaylistChange(t *testing.T) {
	m, err := Compile(Rules{Include: []Pattern{{FieldGroup, "^Sports"}}})
	require.NoError(t, err)

	before := []Entry{{Name: "A", Group: "Sports"}, {Name: "B", Group: "News"}}
	after := append(before, Entry{Name: "C", Group: "Sports HD"})

	count := func(es []Entry) (n int) {
		for _, e := range es {
			if m.Allow(e) {
				n++
			}
		}
		return
	}
	assert.Equal(t, 1, count(before))
	assert.Equal(t, 2, count(after))
}

func TestOrphans(t *testing.T) {
	m, err := Compile(Rules{Pinned: []string{"name:gone", "name:here"}, Blocked: []string{"id:9"}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"name:gone", "id:9"}, m.Orphans([]Entry{{Name: "Here"}}))
}
