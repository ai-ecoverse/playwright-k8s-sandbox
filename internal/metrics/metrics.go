// Package metrics defines the Prometheus instrumentation for the proxy and owns
// the registry exposed on /metrics.
//
// Metrics is a leaf type: it imports none of the other internal packages, so
// session, proxy, and identify can each depend on it without an import cycle.
// Every method is nil-safe — a nil *Metrics is a no-op — so tests and code
// paths that don't care about instrumentation can pass nil freely.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Outcome label values for lifecycle metrics.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// Direction label values for byte-transfer accounting.
const (
	DirClientToBackend = "c2b"
	DirBackendToClient = "b2c"
)

// Lookup result label values.
const (
	LookupCacheHit    = "cache_hit"
	LookupAPIFallback = "api_fallback"
	LookupMiss        = "miss"
)

// Metrics bundles every collector the proxy exports. Construct one with New and
// pass it into the session manager, proxy handler, and identify index.
type Metrics struct {
	reg *prometheus.Registry

	// Session lifecycle.
	sessionsCreated *prometheus.CounterVec   // labels: backend, outcome
	sessionsReaped  *prometheus.CounterVec   // labels: reason
	ensureDuration  *prometheus.HistogramVec // labels: backend, outcome
	ensureFailures  *prometheus.CounterVec   // labels: backend
	sessionLifetime *prometheus.HistogramVec // labels: backend
	sessionIdle     prometheus.Histogram

	// Usage / activity.
	activeConns    prometheus.Gauge
	connDuration   *prometheus.HistogramVec // labels: protocol
	requests       *prometheus.CounterVec   // labels: protocol, code
	requestLatency *prometheus.HistogramVec // labels: protocol
	bytes          *prometheus.CounterVec   // labels: direction

	// Routing / identify.
	lookups        *prometheus.CounterVec // labels: result
	unknownClients prometheus.Counter

	// Errors / health.
	backendDialFailures   prometheus.Counter
	proxyErrors           *prometheus.CounterVec // labels: kind
	backendDeleteFailures prometheus.Counter
}

// New builds a Metrics backed by a fresh registry that also carries the standard
// Go runtime and process collectors plus a build_info gauge.
func New(backend, version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "playwright_build_info",
		Help: "Build metadata for the running proxy; value is always 1.",
	}, []string{"version", "commit", "backend"})
	buildInfo.WithLabelValues(version, commit, backend).Set(1)
	reg.MustRegister(buildInfo)

	m := &Metrics{
		reg: reg,
		sessionsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_sessions_created_total",
			Help: "Sandbox sessions created, by backend and outcome.",
		}, []string{"backend", "outcome"}),
		sessionsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_sessions_reaped_total",
			Help: "Sandbox sessions torn down, by reason (idle, shutdown).",
		}, []string{"reason"}),
		ensureDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "playwright_session_ensure_duration_seconds",
			Help:    "Time spent in backend Ensure (sandbox provisioning / cold start).",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}, []string{"backend", "outcome"}),
		ensureFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_session_ensure_failures_total",
			Help: "Backend Ensure failures, by backend.",
		}, []string{"backend"}),
		sessionLifetime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "playwright_session_lifetime_seconds",
			Help:    "Session lifetime from creation to teardown.",
			Buckets: prometheus.ExponentialBuckets(10, 2, 12), // 10s .. ~11h
		}, []string{"backend"}),
		sessionIdle: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "playwright_session_idle_seconds",
			Help:    "Idle duration of a session at the moment it was reaped.",
			Buckets: prometheus.ExponentialBuckets(30, 2, 10), // 30s .. ~4h
		}),
		activeConns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "playwright_active_connections",
			Help: "Currently open proxied WebSocket connections.",
		}),
		connDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "playwright_connection_duration_seconds",
			Help:    "Duration of a proxied connection (actual sandbox usage time).",
			Buckets: prometheus.ExponentialBuckets(1, 2, 14), // 1s .. ~4.5h
		}, []string{"protocol"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_requests_total",
			Help: "Proxied requests, by protocol and HTTP status code.",
		}, []string{"protocol", "code"}),
		requestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "playwright_request_duration_seconds",
			Help:    "Latency of proxied HTTP/MCP requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"protocol"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_bytes_transferred_total",
			Help: "Bytes copied through proxied connections, by direction.",
		}, []string{"direction"}),
		lookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_lookups_total",
			Help: "Pod-IP to playwright-id lookups, by result.",
		}, []string{"result"}),
		unknownClients: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "playwright_unknown_client_total",
			Help: "Requests rejected because the client pod was not resolvable to a playwright-id.",
		}),
		backendDialFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "playwright_backend_dial_failures_total",
			Help: "Failures dialing the sandbox backend for a WebSocket upgrade.",
		}),
		proxyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "playwright_proxy_errors_total",
			Help: "Proxy-layer errors, by kind.",
		}, []string{"kind"}),
		backendDeleteFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "playwright_backend_delete_failures_total",
			Help: "Failures deleting a sandbox during reaping.",
		}),
	}

	reg.MustRegister(
		m.sessionsCreated, m.sessionsReaped, m.ensureDuration, m.ensureFailures,
		m.sessionLifetime, m.sessionIdle, m.activeConns, m.connDuration,
		m.requests, m.requestLatency, m.bytes, m.lookups, m.unknownClients,
		m.backendDialFailures, m.proxyErrors, m.backendDeleteFailures,
	)
	return m
}

// Registry returns the underlying registry for wiring into the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// RegisterSessionsActive installs a gauge whose value is read from f at scrape
// time (point-in-time count of live sessions). Call once during wiring.
func (m *Metrics) RegisterSessionsActive(f func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "playwright_sessions_active",
		Help: "Sandbox sessions currently tracked by the proxy.",
	}, f))
}

// RegisterRegisteredPods installs a gauge reporting the informer cache size at
// scrape time. Call once during wiring.
func (m *Metrics) RegisterRegisteredPods(f func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "playwright_registered_pods",
		Help: "Labelled client pods currently registered in the identify index.",
	}, f))
}

// --- Session lifecycle ---

func (m *Metrics) SessionCreated(backend, outcome string) {
	if m == nil {
		return
	}
	m.sessionsCreated.WithLabelValues(backend, outcome).Inc()
}

func (m *Metrics) SessionReaped(reason string) {
	if m == nil {
		return
	}
	m.sessionsReaped.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveEnsure(backend, outcome string, seconds float64) {
	if m == nil {
		return
	}
	m.ensureDuration.WithLabelValues(backend, outcome).Observe(seconds)
}

func (m *Metrics) EnsureFailed(backend string) {
	if m == nil {
		return
	}
	m.ensureFailures.WithLabelValues(backend).Inc()
}

func (m *Metrics) ObserveLifetime(backend string, seconds float64) {
	if m == nil {
		return
	}
	m.sessionLifetime.WithLabelValues(backend).Observe(seconds)
}

func (m *Metrics) ObserveIdle(seconds float64) {
	if m == nil {
		return
	}
	m.sessionIdle.Observe(seconds)
}

// --- Usage / activity ---

func (m *Metrics) ConnOpened() {
	if m == nil {
		return
	}
	m.activeConns.Inc()
}

func (m *Metrics) ConnClosed(protocol string, seconds float64) {
	if m == nil {
		return
	}
	m.activeConns.Dec()
	m.connDuration.WithLabelValues(protocol).Observe(seconds)
}

// CountRequest increments the request counter without a latency observation.
// Used for WebSocket upgrades, whose duration is tracked by connection_duration.
func (m *Metrics) CountRequest(protocol, code string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(protocol, code).Inc()
}

// RecordRequest counts a request and observes its latency (HTTP/MCP path).
func (m *Metrics) RecordRequest(protocol, code string, seconds float64) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(protocol, code).Inc()
	m.requestLatency.WithLabelValues(protocol).Observe(seconds)
}

func (m *Metrics) AddBytes(direction string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.bytes.WithLabelValues(direction).Add(float64(n))
}

// --- Routing / identify ---

func (m *Metrics) Lookup(result string) {
	if m == nil {
		return
	}
	m.lookups.WithLabelValues(result).Inc()
}

func (m *Metrics) UnknownClient() {
	if m == nil {
		return
	}
	m.unknownClients.Inc()
}

// --- Errors / health ---

func (m *Metrics) BackendDialFailure() {
	if m == nil {
		return
	}
	m.backendDialFailures.Inc()
}

func (m *Metrics) ProxyError(kind string) {
	if m == nil {
		return
	}
	m.proxyErrors.WithLabelValues(kind).Inc()
}

func (m *Metrics) BackendDeleteFailure() {
	if m == nil {
		return
	}
	m.backendDeleteFailures.Inc()
}
