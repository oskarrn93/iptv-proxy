package iptvproxy

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// route identifies which handler served a request, used as the "route" label
// on requestsTotal/requestDuration. It's a fixed, closed set rather than the
// request path, keeping label cardinality bounded to the handful of routes a
// feed actually serves.
type route string

const (
	routePlaylist route = "playlist"
	routeStream   route = "stream"
	routeEPG      route = "epg"
)

// errorKind identifies why an upstream request failed, used as the "kind"
// label on upstreamErrors.
type errorKind string

const (
	errorKindFetchFailed errorKind = "fetch_failed"
	errorKindHTTPError   errorKind = "http_error"
)

// sessionKind identifies what a /stream request is for, used as the
// "kind" label on activeSessions. It's classified from the URL prefix alone
// (see classifySessionKind), before anything is fetched from upstream.
type sessionKind string

const (
	sessionKindLive  sessionKind = "live"
	sessionKindVOD   sessionKind = "vod"
	sessionKindOther sessionKind = "other"
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
// rather than the global default or one registry shared across feeds:
// registering the same metric names into one registry per feed, then merging
// those registries at scrape time (see Proxy.metricsHandler), keeps a
// registration failure in one feed from taking every feed's /metrics samples
// down with it.
//
// The collectors are always created and always safe to record into, whether
// or not metrics are enabled or registration succeeded — callers never need
// to nil-check before calling Inc()/Observe(). Only exposure via /metrics is
// conditional: a disabled feed or a registration failure (logged, not
// panicked) leaves registry nil, so Proxy.metricsHandler just skips this
// feed's samples instead of the process ever going down over it, since a
// metrics problem should never affect stream serving.
type feedMetrics struct {
	registry         *prometheus.Registry
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	authFailures     prometheus.Counter
	upstreamErrors   *prometheus.CounterVec
	streamReconnects prometheus.Counter
	activeSessions   *prometheus.GaugeVec
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
		activeSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "iptv_proxy_active_sessions",
			Help:        "Currently open /stream requests, by kind (live channel, vod, or other).",
			ConstLabels: constLabels,
		}, []string{labelKind}),
	}

	if !enabled {
		return m
	}

	registry := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{m.requestsTotal, m.requestDuration, m.authFailures, m.upstreamErrors, m.streamReconnects, m.activeSessions} {
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

// sessionStarted/sessionEnded bracket one /stream request's lifetime so
// activeSessions reflects how many are open right now, by kind. Called around
// the full handler (not just the upstream fetch) so a slow or stalled client
// still counts as an open session.
func (m *feedMetrics) sessionStarted(kind sessionKind) {
	m.activeSessions.WithLabelValues(string(kind)).Inc()
}

func (m *feedMetrics) sessionEnded(kind sessionKind) {
	m.activeSessions.WithLabelValues(string(kind)).Dec()
}
