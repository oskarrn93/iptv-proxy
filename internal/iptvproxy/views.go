package iptvproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"

	"github.com/oskarrn93/iptv-proxy/internal/iptvproxy/curation"
)

// View is a separate credential pair that sees only the channels of one feed
// that its Rules allow. It reuses the feed's playlist cache and upstream, so a
// view costs no extra upstream fetches.
type View struct {
	Name        string `json:"name"`
	Feed        string `json:"feed"`
	Credentials `json:",inline"`
	Rules       curation.Rules `json:"rules"`
}

// resolvedView is a View with its rules compiled once, at load/save time.
type resolvedView struct {
	View
	Matcher *curation.Matcher
}

// viewNamePattern keeps names usable as a URL path segment, which the admin
// routes address views by.
var viewNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var (
	errViewNotFound = errors.New("view not found")
	errViewInvalid  = errors.New("invalid view")
)

// viewStore is the runtime-editable set of views, persisted to one JSON file.
// Config feeds are immutable after startup; views are not, so they live here
// behind a lock rather than on Proxy.feeds.
type viewStore struct {
	path string
	// feeds is the set of feed names and credentials a view must not collide
	// with; fixed for the process lifetime.
	feedNames     map[string]struct{}
	feedUsernames map[string]struct{}

	mu    sync.RWMutex
	views []resolvedView
}

type viewsFile struct {
	Views []View `json:"views"`
}

// newViewStore loads path (a missing file is an empty store) and validates
// every stored view against feeds, so a config change that orphans a view
// fails startup loudly instead of silently serving nothing.
func newViewStore(path string, feeds []FeedConfig) (*viewStore, error) {
	s := &viewStore{
		path:          path,
		feedNames:     map[string]struct{}{},
		feedUsernames: map[string]struct{}{},
	}
	for _, f := range feeds {
		s.feedNames[f.Name] = struct{}{}
		s.feedUsernames[f.Username] = struct{}{}
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read views file: %w", err)
	}
	var f viewsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse views file %s: %w", path, err)
	}
	for _, v := range f.Views {
		rv, err := s.resolve(v, s.views)
		if err != nil {
			return nil, fmt.Errorf("views file %s: view %q: %w", path, v.Name, err)
		}
		s.views = append(s.views, rv)
	}
	return s, nil
}

// resolve validates v against the feeds and against others (the views it
// would coexist with) and compiles its rules.
func (s *viewStore) resolve(v View, others []resolvedView) (resolvedView, error) {
	invalid := func(format string, args ...any) (resolvedView, error) {
		return resolvedView{}, fmt.Errorf("%w: %s", errViewInvalid, fmt.Sprintf(format, args...))
	}
	if !viewNamePattern.MatchString(v.Name) {
		return invalid("name must be 1-64 characters of letters, digits, '_', '.' or '-'")
	}
	if v.Username == "" || v.Password == "" {
		return invalid("username and password are required")
	}
	if field := v.Credentials.unsafePathField(); field != "" {
		return invalid("%s must survive a URL path unchanged", field)
	}
	if _, ok := s.feedNames[v.Feed]; !ok {
		return invalid("unknown feed %q", v.Feed)
	}
	// Usernames pick which feed or view serves a request, so they must be
	// unique across both.
	if _, ok := s.feedUsernames[v.Username]; ok {
		return invalid("username %q is already used by a feed", v.Username)
	}
	for _, o := range others {
		if o.Username == v.Username {
			return invalid("username %q is already used by view %q", v.Username, o.Name)
		}
		if o.Name == v.Name {
			return invalid("name %q already exists", v.Name)
		}
	}
	m, err := curation.Compile(v.Rules)
	if err != nil {
		return invalid("%v", err)
	}
	return resolvedView{View: v, Matcher: m}, nil
}

// List returns every view in creation order.
func (s *viewStore) List() []resolvedView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]resolvedView(nil), s.views...)
}

// Get returns the view with the given name.
func (s *viewStore) Get(name string) (resolvedView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.views {
		if v.Name == name {
			return v, nil
		}
	}
	return resolvedView{}, errViewNotFound
}

// Put creates the view, or replaces the existing one of the same name. The
// change is persisted before it becomes visible, so a failed write leaves the
// in-memory set matching the file.
func (s *viewStore) Put(v View) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := slices.IndexFunc(s.views, func(e resolvedView) bool { return e.Name == v.Name })
	others := s.views
	if i >= 0 {
		others = slices.Delete(slices.Clone(s.views), i, i+1)
	}
	rv, err := s.resolve(v, others)
	if err != nil {
		return err
	}
	next := slices.Clone(s.views)
	if i >= 0 {
		next[i] = rv
	} else {
		next = append(next, rv)
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.views = next
	return nil
}

// Delete removes the named view.
func (s *viewStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make([]resolvedView, 0, len(s.views))
	for _, e := range s.views {
		if e.Name != name {
			next = append(next, e)
		}
	}
	if len(next) == len(s.views) {
		return errViewNotFound
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.views = next
	return nil
}

// Match finds the view whose own credentials are exactly creds. candidate is
// the view whose username matched but whose password did not, mirroring
// Proxy.matchFeed so failures can still be attributed.
func (s *viewStore) Match(creds Credentials) (matched, candidate *resolvedView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Put and Delete replace s.views rather than edit it, so a pointer into
	// the current slice stays valid after the lock is released.
	for i := range s.views {
		v := &s.views[i]
		userOK, passOK := v.Credentials.matches(creds)
		if !userOK {
			continue
		}
		if passOK {
			return v, nil
		}
		candidate = v
	}
	return nil, candidate
}

// persist writes views atomically: a crash mid-write leaves the previous file
// intact rather than a truncated one that would fail the next startup.
func (s *viewStore) persist(views []resolvedView) error {
	f := viewsFile{Views: make([]View, len(views))}
	for i, v := range views {
		f.Views[i] = v.View
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write views file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write views file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write views file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write views file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write views file: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("write views file: %w", err)
	}
	return nil
}
