package iptvproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const maxRetries = 3

// upstreamRepository issues HTTP requests to one feed's upstream server. It
// owns the retry/backoff policy for transient network errors and retryable
// status codes, so everything above it deals only in "final response or
// error" terms rather than in individual attempts.
type upstreamRepository struct {
	logger *slog.Logger
	client *http.Client
}

func newUpstreamRepository(logger *slog.Logger, timeout time.Duration) *upstreamRepository {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Tied to the feed's own configured timeout so it never becomes a
		// tighter bottleneck than the per-request context deadline below.
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}
	return &upstreamRepository{
		logger: logger,
		client: &http.Client{Transport: transport},
	}
}

func (u *upstreamRepository) fetch(ctx context.Context, rawURL string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range forwardedRequestHeaders {
		if v := hdr.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	return u.client.Do(req)
}

func isTransientError(err error) bool {
	// net.Error.Timeout rather than the deprecated Temporary ("Temporary errors
	// are not well-defined", Go 1.18). It covers the transport-level deadlines
	// set above, which never surface as context.DeadlineExceeded.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	return false
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (u *upstreamRepository) fetchWithRetry(ctx context.Context, rawURL string, hdr http.Header) (*http.Response, error) {
	logger := loggerFromContext(ctx, u.logger)
	wait := retryDelay
	for attempt := 0; ; attempt++ {
		resp, err := u.fetch(ctx, rawURL, hdr)
		if err != nil {
			if !isTransientError(err) || attempt == maxRetries {
				return nil, err
			}
			logger.WarnContext(ctx, "upstream fetch failed, retrying", "attempt", attempt+1, "max_attempts", maxRetries, "wait", wait, "error", redactErr(err))
		} else {
			if !isRetryableStatus(resp.StatusCode) || attempt == maxRetries {
				return resp, nil
			}
			resp.Body.Close()
			logger.WarnContext(ctx, "upstream retryable response", "status", resp.StatusCode, "wait", wait)
		}
		wait = sleepWithBackoff(wait)
	}
}
