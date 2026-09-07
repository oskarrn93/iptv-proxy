package iptvproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// HealthStatus is the result of probing every feed, independent of how it
// gets rendered to an HTTP response. Keeping it as a plain data type lets
// HealthAggregator's probing logic (Check) be exercised and reasoned about
// without going through ResponseWriter/Request at all.
type HealthStatus struct {
	Healthy   bool     `json:"healthy"`
	Unhealthy []string `json:"unhealthy_feeds,omitempty"`
}

// writeHealthStatus renders a HealthStatus as this project's plain-text
// health response. Shared by the per-feed handler and the aggregator so both
// speak the same wire format from one place.
func writeHealthStatus(w http.ResponseWriter, status HealthStatus) {
	if status.Healthy {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "unhealthy feeds: %s\n", strings.Join(status.Unhealthy, ", "))
}

// StreamRequest is a /stream request that has been parsed and validated
// against this feed's target-safety rules, decoupled from the raw query
// string it came from.
type StreamRequest struct {
	Target *url.URL
}

// StreamRequestError distinguishes a malformed /stream request from one that
// resolves to a disallowed target, so parseStreamRequest can report both
// through a single error return while the handler still maps each to its own
// HTTP status.
type StreamRequestError struct {
	Forbidden bool
	Err       error
}

func (e *StreamRequestError) Error() string { return e.Err.Error() }
func (e *StreamRequestError) Unwrap() error { return e.Err }
