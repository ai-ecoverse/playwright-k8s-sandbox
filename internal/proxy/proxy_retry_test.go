package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend"
	"github.com/carlossg/playwright-k8s-sandbox/internal/metrics"
	"github.com/carlossg/playwright-k8s-sandbox/internal/session"
)

// scriptedResult is one scripted backend.Ensure return value.
type scriptedResult struct {
	ep  backend.Endpoint
	err error
}

// scriptedBackend is a backend.Backend that returns a scripted sequence of
// endpoints per id, simulating a sandbox pod recreated with a new address
// across successive Ensure calls (e.g. a Karpenter eviction).
type scriptedBackend struct {
	mu     sync.Mutex
	calls  map[string]int
	script map[string][]scriptedResult
}

func (b *scriptedBackend) Ensure(ctx context.Context, id string) (backend.Endpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.calls[id]
	b.calls[id] = n + 1
	results := b.script[id]
	if n >= len(results) {
		n = len(results) - 1
	}
	r := results[n]
	return r.ep, r.err
}

func (b *scriptedBackend) callCount(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}

func (b *scriptedBackend) Delete(ctx context.Context, id string) error { return nil }
func (b *scriptedBackend) List(ctx context.Context) ([]string, error)  { return nil, nil }

// deadEndpoint returns the host/port of a listener that has already been
// closed, so dialing it fails fast with connection refused.
func deadEndpoint(t *testing.T) backend.Endpoint {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(addr)
	pn, _ := strconv.Atoi(p)
	return backend.Endpoint{Host: h, Port: pn}
}

// TestHandleWSRetriesOnDialFailure verifies the reactive-recovery path added
// to handleWS: a dial failure against the cached (now-stale) endpoint
// triggers one Sessions.Refresh + retry before giving up.
func TestHandleWSRetriesOnDialFailure(t *testing.T) {
	dead := deadEndpoint(t)
	host, port := fakeBackend(t, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")

	sb := &scriptedBackend{
		calls: map[string]int{},
		script: map[string][]scriptedResult{
			"abc": {
				{ep: dead},
				{ep: backend.Endpoint{Host: host, Port: port}},
			},
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "t", "c")
	mgr := session.New(sb, "test", 2*time.Second, time.Minute, log, m)

	ctx := context.Background()
	sess, err := mgr.Get(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Endpoint != dead {
		t.Fatalf("expected initial session to hold the dead endpoint, got %+v", sess.Endpoint)
	}

	h := &Handler{Sessions: mgr, Log: log, Metrics: m}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleWS(w, r, sess)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 (retry against the fresh endpoint should have succeeded)", resp.StatusCode)
	}
	if got := sb.callCount("abc"); got != 2 {
		t.Fatalf("backend Ensure called %d times, want 2 (initial + refresh)", got)
	}
}

// TestHandleHTTPRetriesOnDialFailure verifies the HTTP/MCP path recovers via
// retryingTransport the same way handleWS does: a connect failure against the
// stale endpoint triggers one Refresh + replay against the fresh endpoint.
func TestHandleHTTPRetriesOnDialFailure(t *testing.T) {
	dead := deadEndpoint(t)
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer backendSrv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backendSrv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	sb := &scriptedBackend{
		calls: map[string]int{},
		script: map[string][]scriptedResult{
			"abc": {
				{ep: dead},
				{ep: backend.Endpoint{Host: host, Port: port}},
			},
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "t", "c")
	mgr := session.New(sb, "test", 2*time.Second, time.Minute, log, m)

	ctx := context.Background()
	sess, err := mgr.Get(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}

	h := New(mgr, nil, "test", log, m)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleHTTP(w, r, sess)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry against the fresh endpoint should have succeeded)", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", string(body), "ok")
	}
	if got := sb.callCount("abc"); got != 2 {
		t.Fatalf("backend Ensure called %d times, want 2 (initial + refresh)", got)
	}
}

// TestHandleHTTPDoesNotRetryOnAppLevelError verifies that an ordinary HTTP
// error response from a healthy backend (no transport failure) is passed
// through unchanged and does not trigger a session refresh.
func TestHandleHTTPDoesNotRetryOnAppLevelError(t *testing.T) {
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer backendSrv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backendSrv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	sb := &scriptedBackend{
		calls: map[string]int{},
		script: map[string][]scriptedResult{
			"abc": {{ep: backend.Endpoint{Host: host, Port: port}}},
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "t", "c")
	mgr := session.New(sb, "test", 2*time.Second, time.Minute, log, m)

	ctx := context.Background()
	sess, err := mgr.Get(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}

	h := New(mgr, nil, "test", log, m)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleHTTP(w, r, sess)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (app-level errors must pass through unchanged)", resp.StatusCode)
	}
	if string(body) != "boom" {
		t.Fatalf("body = %q, want %q", string(body), "boom")
	}
	if got := sb.callCount("abc"); got != 1 {
		t.Fatalf("backend Ensure called %d times, want 1 (no refresh on an app-level error)", got)
	}
}
