package iptvproxy

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// route identifies which handler served a request, used as the "route" label
// on requestsTotal/requestDuration. It's a fixed, closed set rather than the
// request path, keeping label cardinality bounded to the handful of routes a
// feed actually serves.
type route string

const (
	routePlaylist route = "playlist"
	routeStream   route = "stream"
	routeHealth   route = "health"
)

// errorKind identifies why an upstream request failed, used as the "kind"
// label on upstreamErrors.
type errorKind string

const (
	errorKindFetchFailed errorKind = "fetch_failed"
	errorKindHTTPError   errorKind = "http_error"
)

// Metric label names, kept as constants alongside the label values above so
// every string that appears in a metric's identity is named once.
const (
	labelFeed   = "feed"
	labelRoute  = "route"
	labelStatus = "status"
	labelKind   = "kind"
)

// feedMetrics holds one feed's Prometheus collectors on their own registry
// rather than the global default: every feed runs in the same process on its
// own port, and a shared registry would either collide on registration or
// force every feed's samples onto every feed's /metrics endpoint.
//
// The collectors are always created and always safe to record into, whether
// or not metrics are enabled or registration succeeded — callers never need
// to nil-check before calling Inc()/Observe(). Only exposure via /metrics is
// conditional: a disabled feed or a registration failure (logged, not
// panicked) serves a 404 from handler() instead of ever taking the process
// down, since a metrics problem should never affect stream serving.
type feedMetrics struct {
	registry         *prometheus.Registry
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	authFailures     prometheus.Counter
	upstreamErrors   *prometheus.CounterVec
	streamReconnects prometheus.Counter
}

func newFeedMetrics(feed string, enabled bool, logger *slog.Logger) *feedMetrics {
	constLabels := prometheus.Labels{labelFeed: feed}

	m := &feedMetrics{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "iptv_proxy_http_requests_total",
			Help:        "Total HTTP requests handled, by route and status code.",
			ConstLabels: constLabels,
		}, []string{labelRoute, labelStatus}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "iptv_proxy_http_request_duration_seconds",
			Help:        "HTTP request duration in seconds, by route.",
			ConstLabels: constLabels,
			Buckets:     prometheus.DefBuckets,
		}, []string{labelRoute}),
		authFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "iptv_proxy_auth_failures_total",
			Help:        "Total requests rejected for invalid credentials.",
			ConstLabels: constLabels,
		}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "iptv_proxy_upstream_errors_total",
			Help:        "Total upstream errors, by kind (fetch_failed after retries, or http_error status).",
			ConstLabels: constLabels,
		}, []string{labelKind}),
		streamReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "iptv_proxy_stream_reconnects_total",
			Help:        "Total live TS stream reconnects.",
			ConstLabels: constLabels,
		}),
	}

	if !enabled {
		return m
	}

	registry := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{m.requestsTotal, m.requestDuration, m.authFailures, m.upstreamErrors, m.streamReconnects} {
		if err := registry.Register(c); err != nil {
			logger.Error("metrics registration failed, /metrics disabled for this feed", "feed", feed, "error", err)
			return m
		}
	}
	m.registry = registry
	return m
}

// observeRequest records a completed request's outcome under the typed route
// and status, keeping the strconv.Itoa/WithLabelValues plumbing out of
// callers.
func (m *feedMetrics) observeRequest(r route, status int, duration time.Duration) {
	m.requestsTotal.WithLabelValues(string(r), strconv.Itoa(status)).Inc()
	m.requestDuration.WithLabelValues(string(r)).Observe(duration.Seconds())
}

// recordUpstreamError increments upstreamErrors for the given kind.
func (m *feedMetrics) recordUpstreamError(kind errorKind) {
	m.upstreamErrors.WithLabelValues(string(kind)).Inc()
}

// handler serves /metrics, or a 404 when metrics are disabled or failed to
// register — never an error that would need the caller to fall back.
func (m *feedMetrics) handler() http.Handler {
	if m.registry == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics disabled", http.StatusNotFound)
		})
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
