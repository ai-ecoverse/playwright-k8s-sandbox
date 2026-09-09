package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend"
	"github.com/carlossg/playwright-k8s-sandbox/internal/metrics"
)

// ensureResult scripts one Ensure return value.
type ensureResult struct {
	ep  backend.Endpoint
	err error
}

// fakeBackend returns a scripted sequence of Ensure results per id, simulating
// a sandbox pod that gets recreated with a new address across calls (e.g. a
// Karpenter eviction). Calls past the end of the script repeat the last entry.
type fakeBackend struct {
	mu     sync.Mutex
	calls  map[string]int
	script map[string][]ensureResult
}

func (f *fakeBackend) Ensure(ctx context.Context, id string) (backend.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.calls[id]
	f.calls[id] = n + 1
	results := f.script[id]
	if n >= len(results) {
		n = len(results) - 1
	}
	r := results[n]
	return r.ep, r.err
}

func (f *fakeBackend) Delete(ctx context.Context, id string) error { return nil }
func (f *fakeBackend) List(ctx context.Context) ([]string, error)  { return nil, nil }

func newTestManager(t *testing.T, script map[string][]ensureResult) *Manager {
	t.Helper()
	fb := &fakeBackend{calls: map[string]int{}, script: script}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "t", "c")
	return New(fb, "test", 2*time.Second, time.Minute, log, m)
}

func TestGetCreatesAndReusesSession(t *testing.T) {
	mgr := newTestManager(t, map[string][]ensureResult{
		"a": {{ep: backend.Endpoint{Host: "10.0.0.1", Port: 9222}}},
	})
	ctx := context.Background()

	s1, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("expected the same session to be reused across Get calls")
	}
	if s1.Endpoint.Host != "10.0.0.1" {
		t.Fatalf("endpoint = %+v, want host 10.0.0.1", s1.Endpoint)
	}
}

// TestRefreshReResolvesStaleEndpoint is the core regression test: after the
// backend's Ensure result for an id changes (the sandbox pod was recreated
// with a new IP), Refresh must drop the stale session and pick up the new
// endpoint, and a subsequent Get must return the refreshed session.
func TestRefreshReResolvesStaleEndpoint(t *testing.T) {
	mgr := newTestManager(t, map[string][]ensureResult{
		"a": {
			{ep: backend.Endpoint{Host: "10.0.0.1", Port: 9222}},
			{ep: backend.Endpoint{Host: "10.0.0.2", Port: 9222}},
		},
	})
	ctx := context.Background()

	stale, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Endpoint.Host != "10.0.0.1" {
		t.Fatalf("initial endpoint = %+v", stale.Endpoint)
	}

	refreshed, err := mgr.Refresh(ctx, stale)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed == stale {
		t.Fatal("expected Refresh to produce a new session object, not the stale one")
	}
	if refreshed.Endpoint.Host != "10.0.0.2" {
		t.Fatalf("refreshed endpoint = %+v, want the recreated pod's IP", refreshed.Endpoint)
	}

	got, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != refreshed {
		t.Fatal("expected Get to return the refreshed session after Refresh")
	}
}

// TestRefreshOnAlreadyReplacedSessionIsNoop covers the concurrent-callers case:
// once one caller has refreshed a stale session, a second caller still holding
// a reference to the same stale session must not trigger another re-Ensure —
// it should converge on the session the first caller already installed.
func TestRefreshOnAlreadyReplacedSessionIsNoop(t *testing.T) {
	mgr := newTestManager(t, map[string][]ensureResult{
		"a": {
			{ep: backend.Endpoint{Host: "10.0.0.1", Port: 9222}},
			{ep: backend.Endpoint{Host: "10.0.0.2", Port: 9222}},
		},
	})
	ctx := context.Background()

	stale, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}

	r1, err := mgr.Refresh(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mgr.Refresh(ctx, stale) // still passing the same stale session
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Fatal("expected both callers to converge on the same refreshed session")
	}
}

func TestRefreshPropagatesEnsureFailure(t *testing.T) {
	mgr := newTestManager(t, map[string][]ensureResult{
		"a": {
			{ep: backend.Endpoint{Host: "10.0.0.1", Port: 9222}},
			{err: errors.New("sandbox gone")},
		},
	})
	ctx := context.Background()

	stale, err := mgr.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Refresh(ctx, stale); err == nil {
		t.Fatal("expected Refresh to propagate the backend Ensure failure")
	}
}
