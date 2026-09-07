package iptvproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// HealthAggregator serves one /health endpoint that reports on every feed by
// probing each feed's own /health route over loopback, so an operator (and
// the container healthcheck) has a single port to poll instead of one per
// feed.
type HealthAggregator struct {
	names   []string
	targets []string // "http://127.0.0.1:<port>/health", one per feed, same order as names
	client  *http.Client
}

// NewHealthAggregator builds an aggregator for feeds, resolving each feed's
// own loopback health URL from its listen_addr.
func NewHealthAggregator(feeds []FeedConfig) (*HealthAggregator, error) {
	names := make([]string, len(feeds))
	targets := make([]string, len(feeds))
	for i, f := range feeds {
		_, port, err := net.SplitHostPort(f.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("feed %q: invalid listen_addr %q: %w", f.Name, f.ListenAddr, err)
		}
		names[i] = f.Name
		targets[i] = "http://" + net.JoinHostPort("127.0.0.1", port) + "/health"
	}
	return &HealthAggregator{
		names:   names,
		targets: targets,
		client:  &http.Client{Timeout: 5 * time.Second},
	}, nil
}

// Check probes every feed and reports the aggregate result as a HealthStatus,
// with no knowledge of HTTP at all — ServeHTTP is the only place that
// translates this into a response.
func (a *HealthAggregator) Check(ctx context.Context) HealthStatus {
	var (
		mu        sync.Mutex
		unhealthy []string
		wg        sync.WaitGroup
	)
	for i, target := range a.targets {
		wg.Add(1)
		go func(name, target string) {
			defer wg.Done()
			if !a.probe(ctx, target) {
				mu.Lock()
				unhealthy = append(unhealthy, name)
				mu.Unlock()
			}
		}(a.names[i], target)
	}
	wg.Wait()

	// Sorted so the response is stable across runs rather than depending on
	// goroutine scheduling order.
	sort.Strings(unhealthy)
	return HealthStatus{Healthy: len(unhealthy) == 0, Unhealthy: unhealthy}
}

func (a *HealthAggregator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	writeHealthStatus(w, a.Check(r.Context()))
}

func (a *HealthAggregator) probe(ctx context.Context, target string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
