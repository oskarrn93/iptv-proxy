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
// misses via singleflight.
func newSingleflightCache[T any](ttl time.Duration) *singleflightCache[T] {
	return &singleflightCache[T]{ttl: ttl}
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

// lastStored returns the most recently stored value regardless of age.
func (c *singleflightCache[T]) lastStored() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, !c.cachedAt.IsZero()
}

// getOrFetch returns the cached value, or runs fn on a miss and caches its
// result. If fn fails it falls back to the last stored value however old, so
// an upstream outage doesn't take down a feed that was serving fine. Storing
// inside the flight closes the window where a request landing just after the
// flight ended would refetch before the result was stored.
// cached reports a cache hit; shared reports that this call rode along on
// another in-flight fetch.
func (c *singleflightCache[T]) getOrFetch(fn func() (T, error)) (result T, cached, shared bool, err error) {
	if v, ok := c.get(); ok {
		return v, true, false, nil
	}
	v, err, shared := c.group.Do("", func() (any, error) {
		v, err := fn()
		if err == nil {
			c.store(v)
			return v, nil
		}
		if stale, ok := c.lastStored(); ok {
			return stale, nil
		}
		return v, err
	})
	if err != nil {
		var zero T
		return zero, false, shared, err
	}
	return v.(T), false, shared, nil
}
