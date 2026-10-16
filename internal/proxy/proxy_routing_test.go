package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend"
	"github.com/carlossg/playwright-k8s-sandbox/internal/metrics"
	"github.com/carlossg/playwright-k8s-sandbox/internal/session"
)

func TestTrustedActorRouting(t *testing.T) {
	for _, protocol := range []string{"http", "ws"} {
		for _, actor := range []string{"", "trusted/pw-alpha"} {
			t.Run(protocol+"/"+actor, func(t *testing.T) {
				seen := make(chan string, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Get("ate-target-actor")
					w.WriteHeader(http.StatusTeapot)
				}))
				defer upstream.Close()
				host, portString, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
				port, _ := strconv.Atoi(portString)
				sess := &session.Session{ID: "alpha", Endpoint: backend.Endpoint{Host: host, Port: port, TargetActor: actor}}
				log := slog.New(slog.NewTextHandler(io.Discard, nil))
				h := New(nil, nil, "substrate", log, metrics.New("substrate", "test", "test"))
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if protocol == "ws" {
						h.handleWS(w, r, sess)
					} else {
						h.handleHTTP(w, r, sess)
					}
				}))
				defer proxy.Close()
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL, nil)
				req.Header.Set("ate-target-actor", "attacker/pw-victim")
				if protocol == "ws" {
					req.Header.Set("Connection", "Upgrade")
					req.Header.Set("Upgrade", "websocket")
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if got := <-seen; got != actor {
					t.Fatalf("forwarded actor=%q, want %q", got, actor)
				}
			})
		}
	}
}
