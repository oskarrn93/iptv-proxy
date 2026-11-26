package iptvproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

func testViewStore(t *testing.T) (*viewStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "views.json")
	feeds := []FeedConfig{{Name: "main", Credentials: Credentials{Username: "alice", Password: "secret"}}}
	s, err := newViewStore(path, feeds)
	require.NoError(t, err)
	return s, path
}

func testView(name, user string) View {
	return View{Name: name, Feed: "main", Credentials: Credentials{Username: user, Password: "pw"}}
}

func TestViewStore_PersistsAndReloads(t *testing.T) {
	s, path := testViewStore(t)
	v := testView("kids", "bob")
	v.Rules = curation.Rules{Include: []curation.Pattern{{Field: curation.FieldGroup, Regex: "Kids"}}, Pinned: []string{"name:x"}}
	require.NoError(t, s.Put(v))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	reloaded, err := newViewStore(path, []FeedConfig{{Name: "main", Credentials: Credentials{Username: "alice", Password: "secret"}}})
	require.NoError(t, err)
	got, err := reloaded.Get("kids")
	require.NoError(t, err)
	assert.Equal(t, v, got.View)
	assert.True(t, got.Matcher.Allow(curation.Entry{Name: "Cartoon", Group: "Kids"}))
}

func TestViewStore_MissingFileIsEmpty(t *testing.T) {
	s, _ := testViewStore(t)
	assert.Empty(t, s.List())
}

func TestViewStore_PutReplacesByName(t *testing.T) {
	s, _ := testViewStore(t)
	require.NoError(t, s.Put(testView("kids", "bob")))
	// Re-saving under the same name must not trip the username-uniqueness
	// check against itself.
	updated := testView("kids", "bob")
	updated.Password = "new"
	require.NoError(t, s.Put(updated))
	require.Len(t, s.List(), 1)
	assert.Equal(t, "new", s.List()[0].Password)
}

func TestViewStore_Validation(t *testing.T) {
	s, _ := testViewStore(t)
	require.NoError(t, s.Put(testView("kids", "bob")))

	bad := func(mut func(*View)) View { v := testView("other", "carol"); mut(&v); return v }
	tests := map[string]View{
		"no name":              bad(func(v *View) { v.Name = "" }),
		"name with slash":      bad(func(v *View) { v.Name = "a/b" }),
		"no password":          bad(func(v *View) { v.Password = "" }),
		"unknown feed":         bad(func(v *View) { v.Feed = "nope" }),
		"username of feed":     bad(func(v *View) { v.Username = "alice" }),
		"username of view":     bad(func(v *View) { v.Username = "bob" }),
		"path-unsafe password": bad(func(v *View) { v.Password = "a/b" }),
		"bad regex":            bad(func(v *View) { v.Rules.Include = []curation.Pattern{{Field: curation.FieldName, Regex: "("}} }),
	}
	for name, v := range tests {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, s.Put(v), errViewInvalid)
			assert.Len(t, s.List(), 1)
		})
	}
}

func TestViewStore_DeleteAndNotFound(t *testing.T) {
	s, path := testViewStore(t)
	require.NoError(t, s.Put(testView("kids", "bob")))
	require.NoError(t, s.Delete("kids"))
	assert.Empty(t, s.List())
	assert.ErrorIs(t, s.Delete("kids"), errViewNotFound)
	_, err := s.Get("kids")
	assert.ErrorIs(t, err, errViewNotFound)

	reloaded, err := newViewStore(path, nil)
	require.NoError(t, err)
	assert.Empty(t, reloaded.List())
}

func TestViewStore_FailedWriteLeavesMemoryUntouched(t *testing.T) {
	dir := t.TempDir()
	s, err := newViewStore(filepath.Join(dir, "gone", "views.json"), []FeedConfig{{Name: "main"}})
	require.NoError(t, err)
	assert.Error(t, s.Put(testView("kids", "bob")))
	assert.Empty(t, s.List())
}

func TestViewStore_LoadRejectsOrphanedView(t *testing.T) {
	s, path := testViewStore(t)
	require.NoError(t, s.Put(testView("kids", "bob")))

	_, err := newViewStore(path, []FeedConfig{{Name: "renamed", Credentials: Credentials{Username: "alice", Password: "secret"}}})
	assert.ErrorContains(t, err, `unknown feed "main"`)
}

func TestViewStore_LoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "views.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, err := newViewStore(path, nil)
	assert.ErrorContains(t, err, "parse views file")
}

func TestViewStore_Match(t *testing.T) {
	s, _ := testViewStore(t)
	require.NoError(t, s.Put(testView("kids", "bob")))

	m, c := s.Match(Credentials{Username: "bob", Password: "pw"})
	require.NotNil(t, m)
	assert.Nil(t, c)
	assert.Equal(t, "kids", m.Name)

	m, c = s.Match(Credentials{Username: "bob", Password: "wrong"})
	assert.Nil(t, m)
	require.NotNil(t, c)
	assert.Equal(t, "kids", c.Name)

	m, c = s.Match(Credentials{Username: "alice", Password: "secret"})
	assert.Nil(t, m)
	assert.Nil(t, c)
}
