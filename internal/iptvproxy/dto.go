package iptvproxy

import (
	"net/url"
)

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
