package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend"
	"github.com/carlossg/playwright-k8s-sandbox/internal/metrics"
	"github.com/carlossg/playwright-k8s-sandbox/internal/session"
)

// fakeBackend accepts one connection, reads the upgrade request, and replies
// with the supplied raw HTTP response. Returns the host:port to dial.
func fakeBackend(t *testing.T, rawResponse string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for { // consume request line + headers up to the blank line
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		io.WriteString(conn, rawResponse)
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := strconv.Atoi(p)
	return h, pn
}

func metricsText(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestHandleWSRejectedUpgrade verifies that when the backend rejects the
// upgrade (non-101), the proxy forwards the real status to the client, records
// it under that code — not a bogus 101 — and does not count a live connection.
func TestHandleWSRejectedUpgrade(t *testing.T) {
	host, port := fakeBackend(t, "HTTP/1.1 400 Bad Request\r\nContent-Length: 11\r\nConnection: close\r\n\r\nbad request")

	m := metrics.New("test", "t", "c")
	h := &Handler{
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
	}
	sess := &session.Session{ID: "abc", Endpoint: backend.Endpoint{Host: host, Port: port}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleWS(w, r, sess)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client status = %d, want 400", resp.StatusCode)
	}
	if string(body) != "bad request" {
		t.Errorf("client body = %q, want %q", string(body), "bad request")
	}

	text := metricsText(t, m)
	if !strings.Contains(text, `playwright_requests_total{code="400",protocol="ws"} 1`) {
		t.Errorf("expected a ws/400 request counter, got:\n%s", text)
	}
	if strings.Contains(text, `code="101"`) {
		t.Errorf("rejected upgrade should not be recorded as 101:\n%s", text)
	}
	if !strings.Contains(text, "playwright_active_connections 0") {
		t.Errorf("rejected upgrade should not leave an active connection:\n%s", text)
	}
}

// TestHandleWSAcceptedUpgrade verifies the happy path: a 101 from the backend
// is spliced through, bytes flow both ways, and the connection is accounted.
func TestHandleWSAcceptedUpgrade(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		// Echo one framed payload then close.
		buf := make([]byte, 5)
		io.ReadFull(br, buf)
		conn.Write([]byte("PONG!"))
	}()
	host, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	m := metrics.New("test", "t", "c")
	h := &Handler{
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
	}
	sess := &session.Session{ID: "abc", Endpoint: backend.Endpoint{Host: host, Port: port}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleWS(w, r, sess)
	}))
	defer srv.Close()

	// Raw client so we can drive the tunnel after the handshake.
	u := strings.TrimPrefix(srv.URL, "http://")
	c, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	cbr := bufio.NewReader(c)
	status, err := cbr.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("expected 101 handshake, got %q", status)
	}
	for { // drain handshake headers
		line, _ := cbr.ReadString('\n')
		if line == "\r\n" || line == "\n" || line == "" {
			break
		}
	}
	io.WriteString(c, "PING!")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	out := make([]byte, 5)
	if _, err := io.ReadFull(cbr, out); err != nil {
		t.Fatalf("reading tunnel reply: %v", err)
	}
	if string(out) != "PONG!" {
		t.Errorf("tunnel reply = %q, want PONG!", string(out))
	}
	c.Close()

	// Give the deferred accounting a moment to run after the conn closes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(metricsText(t, m), "playwright_active_connections 0") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := metricsText(t, m)
	if !strings.Contains(text, `playwright_requests_total{code="101",protocol="ws"} 1`) {
		t.Errorf("expected a ws/101 request counter, got:\n%s", text)
	}
	if !strings.Contains(text, "playwright_active_connections 0") {
		t.Errorf("connection should be closed out, got:\n%s", text)
	}
}
