package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNilMetricsAreNoops(t *testing.T) {
	var m *Metrics // nil
	// None of these should panic on a nil receiver.
	m.SessionCreated("isola", OutcomeSuccess)
	m.SessionReaped("idle")
	m.ObserveEnsure("isola", OutcomeSuccess, 1.5)
	m.EnsureFailed("isola")
	m.ObserveLifetime("isola", 100)
	m.ObserveIdle(60)
	m.ConnOpened()
	m.ConnClosed("ws", 30)
	m.RecordRequest("http", "200", 0.2)
	m.AddBytes(DirClientToBackend, 1024)
	m.Lookup(LookupCacheHit)
	m.UnknownClient()
	m.BackendDialFailure()
	m.ProxyError("upstream")
	m.BackendDeleteFailure()
	m.RegisterSessionsActive(func() float64 { return 0 })
	m.RegisterRegisteredPods(func() float64 { return 0 })
	if m.Registry() != nil {
		t.Fatal("nil Metrics should return a nil registry")
	}
}

func TestCountersRecord(t *testing.T) {
	m := New("isola", "test", "abc123")

	m.SessionCreated("isola", OutcomeSuccess)
	m.SessionCreated("isola", OutcomeSuccess)
	m.SessionCreated("isola", OutcomeFailure)
	m.SessionReaped("idle")
	m.UnknownClient()
	m.Lookup(LookupCacheHit)
	m.AddBytes(DirClientToBackend, 500)
	m.AddBytes(DirClientToBackend, 500)

	if got := testutil.ToFloat64(m.sessionsCreated.WithLabelValues("isola", OutcomeSuccess)); got != 2 {
		t.Errorf("sessions_created success = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.sessionsCreated.WithLabelValues("isola", OutcomeFailure)); got != 1 {
		t.Errorf("sessions_created failure = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.unknownClients); got != 1 {
		t.Errorf("unknown_client = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.bytes.WithLabelValues(DirClientToBackend)); got != 1000 {
		t.Errorf("bytes c2b = %v, want 1000", got)
	}
}

func TestBuildInfoAndGauges(t *testing.T) {
	m := New("substrate", "v1.2.3", "deadbeef")
	m.RegisterSessionsActive(func() float64 { return 3 })

	got, err := testutil.GatherAndCount(m.Registry(),
		"playwright_build_info", "playwright_sessions_active")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("expected 2 metric families, got %d", got)
	}
}

// TestMetricsEndpointExposition drives the actual /metrics HTTP handler to
// confirm registered series are exported in the Prometheus text format.
func TestMetricsEndpointExposition(t *testing.T) {
	m := New("isola", "v1", "abc")
	m.RegisterSessionsActive(func() float64 { return 5 })
	m.RegisterRegisteredPods(func() float64 { return 2 })
	m.SessionCreated("isola", OutcomeSuccess)
	m.RecordRequest("http", "200", 0.1)

	srv := httptest.NewServer(promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		"playwright_sessions_created_total",
		"playwright_sessions_active 5",
		"playwright_registered_pods 2",
		"playwright_requests_total",
		"playwright_build_info",
		"go_goroutines",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}
