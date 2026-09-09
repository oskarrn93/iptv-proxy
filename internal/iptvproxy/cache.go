package iptvproxy

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// singleflightCache holds one upstream fetch result behind a TTL cache and a
// singleflight.Group, coalescing concurrent callers who miss the cache onto a
// single fetch. It backs both the playlist and EPG routes (see
// feedService.playlistCache/epgCache), which otherwise repeated this same
// mutex/cache/singleflight shape for two unrelated result types.
//
// A feedService has exactly one fetch target per cache (its playlist, or its
// EPG guide), so every call coalesces under the same fixed key rather than one
// derived per-request.
type singleflightCache[T any] struct {
	ttl time.Duration
	key string

	group singleflight.Group

	// mu guards value/cachedAt. Reads and writes happen outside group's own
	// critical section (a cache hit skips group.Do entirely), so they need
	// their own lock rather than riding on singleflight's.
	mu       sync.Mutex
	value    T
	cachedAt time.Time
}

// newSingleflightCache builds a cache that serves a fetched value for ttl
// before the next request triggers a fresh fetch, coalescing concurrent
// misses via singleflight under key.
func newSingleflightCache[T any](ttl time.Duration, key string) *singleflightCache[T] {
	return &singleflightCache[T]{ttl: ttl, key: key}
}

// get returns the cached value if one has been stored and is still within
// ttl. A zero cachedAt means nothing has been cached yet, so no separate
// "is set" flag is needed.
func (c *singleflightCache[T]) get() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedAt.IsZero() || time.Since(c.cachedAt) > c.ttl {
		var zero T
		return zero, false
	}
	return c.value, true
}

// store records a successful fetch for get to serve until it ages out.
func (c *singleflightCache[T]) store(v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = v
	c.cachedAt = time.Now()
}

// backdate pushes the cached entry's timestamp back by d, so a test can make
// it look stale without sleeping ttl.
func (c *singleflightCache[T]) backdate(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedAt = c.cachedAt.Add(-d)
}

// fetch runs fn, coalescing any other concurrent fetch call onto the same
// invocation via the singleflight group. shared reports whether this
// particular call rode along on another in-flight fetch rather than
// triggering its own. It does not consult or update the cache — callers
// combine get/fetch/store themselves (see feedService.servePlaylist) so the
// cache-hit path can skip the singleflight group entirely.
func (c *singleflightCache[T]) fetch(fn func() (T, error)) (result T, shared bool, err error) {
	v, err, shared := c.group.Do(c.key, func() (any, error) {
		return fn()
	})
	if err != nil {
		var zero T
		return zero, shared, err
	}
	return v.(T), shared, nil
}
