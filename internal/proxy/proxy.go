// Package proxy is the HTTP/WebSocket reverse proxy. The hot path:
//
//  1. Extract the client pod IP from the connection.
//  2. Look up the playwright-id in the informer-backed index.
//  3. Get-or-create the session for that id (singleflight via session.Manager).
//  4. Route the request to the session's Endpoint, copying bytes both ways and
//     bumping the session's lastActive on every request / open connection.
//
// HTTP and WS share the same handler. WS is detected by the Upgrade header.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend"
	"github.com/carlossg/playwright-k8s-sandbox/internal/identify"
	"github.com/carlossg/playwright-k8s-sandbox/internal/metrics"
	"github.com/carlossg/playwright-k8s-sandbox/internal/session"
)

// maxReplayBodyBytes caps how much of a request body handleHTTP buffers so it
// can be replayed by retryingTransport after a dial-failure retry. Bodies
// larger than this are streamed through unbuffered (no retry on failure).
const maxReplayBodyBytes int64 = 1 << 20 // 1 MiB

type Handler struct {
	Sessions  *session.Manager
	Identify  *identify.Index
	GetCtx    func(*http.Request) (context.Context, context.CancelFunc)
	Log       *slog.Logger
	Backend   string // "sandboxclaim" or "substrate"; controls actor-header injection
	Metrics   *metrics.Metrics
	httpProxy *httputil.ReverseProxy
}

func New(sessions *session.Manager, idx *identify.Index, backendKind string, log *slog.Logger, m *metrics.Metrics) *Handler {
	h := &Handler{
		Sessions: sessions,
		Identify: idx,
		Log:      log,
		Backend:  backendKind,
		Metrics:  m,
	}
	h.httpProxy = &httputil.ReverseProxy{
		Director:      func(*http.Request) {}, // we rewrite in ServeHTTP before calling
		ErrorHandler:  h.proxyError,
		FlushInterval: -1, // immediate flush for streaming / SSE / MCP
		Transport: &retryingTransport{
			inner:       http.DefaultTransport,
			sessions:    sessions,
			backendKind: backendKind,
		},
	}
	return h
}

// sessCtxKey stashes the session a request was proxied for so retryingTransport
// can re-resolve it without threading extra parameters through
// httputil.ReverseProxy.
type sessCtxKey struct{}

// retryingTransport gives the HTTP/MCP path the same one-shot recovery as the
// WebSocket path (see the dial-failure handling in handleWS): if the round trip
// fails to even connect, the cached endpoint is likely stale because the
// sandbox pod was recreated with a new IP. It re-resolves the session once and
// replays the request against the fresh endpoint before giving up.
//
// Only a genuine dial/connect failure triggers this (see isDialFailure); a
// request that reached the backend and then failed (broken connection mid-read,
// timeout, or an HTTP error status) is not retried here, since the backend may
// already have acted on it — ordinary app/page-level errors must not trigger
// failover.
type retryingTransport struct {
	inner       http.RoundTripper
	sessions    *session.Manager
	backendKind string
}

func (t *retryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil || !isDialFailure(err) {
		return resp, err
	}
	sess, ok := req.Context().Value(sessCtxKey{}).(*session.Session)
	if !ok {
		return resp, err
	}
	hasBody := req.Body != nil && req.Body != http.NoBody
	if hasBody && req.GetBody == nil {
		return resp, err
	}
	refreshed, rerr := t.sessions.Refresh(req.Context(), sess)
	if rerr != nil {
		return resp, err
	}
	req2 := req.Clone(req.Context())
	if req.GetBody != nil {
		body, berr := req.GetBody()
		if berr != nil {
			return resp, err
		}
		req2.Body = body
	}
	req2.URL.Host = refreshed.Endpoint.MCPAddr()
	req2.Host = substrateHostOrDefault(t.backendKind, refreshed.ID, req2.URL.Host)
	return t.inner.RoundTrip(req2)
}

// isDialFailure reports whether err represents a failure to establish the
// connection at all (as opposed to a failure after connecting), by checking
// for a *net.OpError with Op == "dial". Only this class of failure implies the
// cached endpoint itself is unreachable and worth re-resolving.
func isDialFailure(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientIP, err := clientIPFromReq(r)
	if err != nil {
		http.Error(w, "cannot determine client IP", http.StatusBadRequest)
		return
	}

	id, ok := h.Identify.Lookup(clientIP)
	if !ok {
		h.Metrics.UnknownClient()
		h.Log.Warn("unknown client", "ip", clientIP, "path", r.URL.Path)
		http.Error(w, fmt.Sprintf("client pod %s not labelled with playwright-id", clientIP), http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sess, err := h.Sessions.Get(ctx, id)
	if err != nil {
		h.Log.Error("session get failed", "id", id, "err", err)
		http.Error(w, fmt.Sprintf("session unavailable: %v", err), http.StatusBadGateway)
		return
	}

	sess.MarkActive()

	if isWebsocketUpgrade(r) {
		h.handleWS(w, r, sess)
		return
	}
	h.handleHTTP(w, r, sess)
}

func (h *Handler) handleHTTP(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	// Rewrite the request to point at the sandbox's MCP-over-HTTP endpoint.
	// This is a distinct port from the WS endpoint (Endpoint.Addr) because
	// @playwright/mcp cannot share a process/port with chromium.launchServer;
	// MCPAddr falls back to the WS port for single-port sandboxes.
	target := &url.URL{Scheme: "http", Host: sess.Endpoint.MCPAddr()}
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = target.Scheme
	r2.URL.Host = target.Host
	r2.Host = substrateHostOrDefault(h.Backend, sess.ID, target.Host)
	r2.RequestURI = ""

	// Buffer up to maxReplayBodyBytes and set GetBody so retryingTransport can
	// replay the request once if the cached endpoint turns out to be stale (see
	// retryingTransport.RoundTrip). MCP-over-HTTP payloads are small
	// control-plane calls, so buffering them is cheap; a body larger than the
	// cap is streamed through unbuffered and simply won't be retried.
	if r2.Body != nil && r2.Body != http.NoBody {
		buf, readErr := io.ReadAll(io.LimitReader(r2.Body, maxReplayBodyBytes+1))
		if readErr != nil {
			if closeErr := r2.Body.Close(); closeErr != nil {
				h.Log.Warn("closing request body", "err", closeErr)
			}
			http.Error(w, fmt.Sprintf("reading request body: %v", readErr), http.StatusBadGateway)
			return
		}
		if int64(len(buf)) > maxReplayBodyBytes {
			r2.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r2.Body))
		} else {
			if closeErr := r2.Body.Close(); closeErr != nil {
				h.Log.Warn("closing request body", "err", closeErr)
			}
			r2.Body = io.NopCloser(bytes.NewReader(buf))
			r2.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(buf)), nil
			}
		}
	}
	r2 = r2.WithContext(context.WithValue(r2.Context(), sessCtxKey{}, sess))

	sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	h.httpProxy.ServeHTTP(sr, r2)
	h.Metrics.RecordRequest("http", strconv.Itoa(sr.status), time.Since(start).Seconds())
	sess.MarkActive()
}

// statusRecorder captures the response status code the ReverseProxy writes so
// it can be used as a metric label.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	// Hijack the client connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "server does not support hijacking", http.StatusInternalServerError)
		return
	}

	// Dial the backend. A dial failure against an otherwise-healthy session means
	// the sandbox pod was most likely recreated with a new IP underneath the
	// cached endpoint (e.g. a Karpenter eviction) — re-resolve once via the
	// backend and retry before giving up.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	backendConn, err := dialer.DialContext(r.Context(), "tcp", sess.Endpoint.Addr())
	if err != nil {
		h.Metrics.BackendDialFailure()
		if refreshed, rerr := h.Sessions.Refresh(r.Context(), sess); rerr == nil {
			sess = refreshed
			backendConn, err = dialer.DialContext(r.Context(), "tcp", sess.Endpoint.Addr())
			if err != nil {
				h.Metrics.BackendDialFailure()
			}
		}
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("backend dial failed: %v", err), http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Forward the upgrade request to the backend (preserving headers, with
	// X-Forwarded-For and a substrate-routable Host when applicable).
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = sess.Endpoint.Addr()
	outReq.Host = substrateHostOrDefault(h.Backend, sess.ID, sess.Endpoint.Addr())
	outReq.Header.Set("X-Forwarded-For", clientIPMust(r))
	outReq.Header.Set("Host", outReq.Host)
	if err := outReq.Write(backendConn); err != nil {
		http.Error(w, fmt.Sprintf("backend write failed: %v", err), http.StatusBadGateway)
		return
	}

	// Read the backend's response to the upgrade so we can record its real
	// status and only tunnel when the upgrade was actually accepted. Bytes the
	// reader buffers past the response headers are carried into the tunnel via
	// backendBr below, so nothing is lost.
	backendBr := bufio.NewReader(backendConn)
	resp, err := http.ReadResponse(backendBr, outReq)
	if err != nil {
		h.Metrics.ProxyError("ws_upstream")
		h.Log.Warn("backend upgrade response failed", "id", sess.ID, "err", err)
		http.Error(w, fmt.Sprintf("backend response failed: %v", err), http.StatusBadGateway)
		return
	}
	h.Metrics.CountRequest("ws", strconv.Itoa(resp.StatusCode))

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// Upgrade rejected. Forward the backend's response to the client verbatim
		// and stop — this is not a live connection, so no ConnOpened / active
		// connection accounting.
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	// Upgrade accepted: hijack the client connection and splice the two.
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		h.Log.Error("hijack failed", "err", err)
		return
	}
	defer clientConn.Close()

	// Replay the 101 handshake to the client. We serialize the status line and
	// headers by hand rather than resp.Write, which would try to copy resp.Body
	// (the tunnel) and deadlock.
	var handshake bytes.Buffer
	fmt.Fprintf(&handshake, "%s %s\r\n", resp.Proto, resp.Status)
	resp.Header.Write(&handshake)
	handshake.WriteString("\r\n")
	if _, err := clientConn.Write(handshake.Bytes()); err != nil {
		h.Log.Error("write upgrade response failed", "err", err)
		return
	}

	sess.ConnOpened()
	h.Metrics.ConnOpened()
	start := time.Now()
	var c2b, b2c int64
	defer func() {
		sess.ConnClosed()
		h.Metrics.ConnClosed("ws", time.Since(start).Seconds())
		h.Metrics.AddBytes(metrics.DirClientToBackend, atomic.LoadInt64(&c2b))
		h.Metrics.AddBytes(metrics.DirBackendToClient, atomic.LoadInt64(&b2c))
	}()

	// Pump bytes both directions until either side closes.
	errc := make(chan error, 2)
	go func() {
		// Drain anything already buffered in the bufio.Reader, then stream.
		if brw != nil && brw.Reader.Buffered() > 0 {
			if n, err := io.CopyN(backendConn, brw.Reader, int64(brw.Reader.Buffered())); err != nil {
				atomic.AddInt64(&c2b, n)
				errc <- err
				return
			} else {
				atomic.AddInt64(&c2b, n)
			}
		}
		n, err := io.Copy(backendConn, clientConn)
		atomic.AddInt64(&c2b, n)
		errc <- err
	}()
	go func() {
		// Read from backendBr, not backendConn, so bytes buffered past the
		// response headers are forwarded.
		n, err := io.Copy(clientConn, backendBr)
		atomic.AddInt64(&b2c, n)
		errc <- err
	}()
	// Wait for one direction to end, then close both connections so the other
	// io.Copy unblocks, and drain its result. This guarantees both byte totals
	// are final before the deferred handler reads them (correct accounting for
	// half-closed connections).
	<-errc
	clientConn.Close()
	backendConn.Close()
	<-errc
}

func (h *Handler) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	h.Metrics.ProxyError("upstream")
	h.Log.Warn("upstream error", "path", r.URL.Path, "err", err)
	http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
}

func isWebsocketUpgrade(r *http.Request) bool {
	conn := r.Header.Get("Connection")
	upg := r.Header.Get("Upgrade")
	return strings.Contains(strings.ToLower(conn), "upgrade") && strings.EqualFold(upg, "websocket")
}

func clientIPFromReq(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", err
	}
	return host, nil
}

func clientIPMust(r *http.Request) string {
	ip, _ := clientIPFromReq(r)
	return ip
}

// substrateHostOrDefault returns the actor-routable Host header for substrate
// or the upstream addr for everything else.
func substrateHostOrDefault(backendKind, playwrightID, fallback string) string {
	if backendKind == "substrate" {
		return "pw-" + playwrightID + "." + backend.SubstrateActorHost
	}
	return fallback
}
