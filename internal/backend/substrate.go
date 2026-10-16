// Substrate backend: talks to substrate's Control gRPC service on
// ate-api-server to manage actor lifecycles. Data-plane traffic still flows
// through this proxy using a trusted ate-target-actor routing header,
// which substrate already runs as a multiplexing data plane.
//
// One actor per playwright-id. Ensure is idempotent: GetActor first, CreateActor
// on NotFound. Delete suspends then deletes (substrate only deletes suspended
// actors).
package backend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend/ateapipb"
)

// readyPollInterval is how often Ensure polls GetActor while waiting for the
// actor to reach ACTOR_STATE_RUNNING. A missed deadline is an error.
const (
	readyPollInterval = 200 * time.Millisecond
	readyDeadline     = 60 * time.Second
	// Running state can precede the application's listening socket after a
	// restore. Probe the data plane before forwarding the caller's upgrade.
	upstreamReadyDeadline = 30 * time.Second
	upstreamProbeInterval = 200 * time.Millisecond
)

type Substrate struct {
	control           ateapipb.ControlClient
	conn              *grpc.ClientConn
	actorTemplateNS   string
	actorTemplateName string
	atenetRouterHost  string
	atenetRouterPort  int
	probeClient       *http.Client
	allowedHTTPHosts  []string
}

// NewSubstrate dials the ate-api-server's gRPC endpoint and returns a backend
// that creates one actor per playwright-id. apiEndpoint is "host:port"; the
// proxy is expected to talk to ate-api-server via TLS but the certs are
// self-signed inside the cluster, hence InsecureSkipVerify (consistent with
// kubectl-ate's behavior).
// NewSubstrate is the convenience constructor that defaults forceBoot=false.
func NewSubstrate(apiEndpoint, atenetRouterAddr, actorTemplateRef string) (*Substrate, error) {
	return NewSubstrateWithOptions(apiEndpoint, atenetRouterAddr, actorTemplateRef, false)
}

// NewSubstrateWithOptions rejects the legacy boot override removed in v0.5.
func NewSubstrateWithOptions(apiEndpoint, atenetRouterAddr, actorTemplateRef string, forceBoot bool, allowedHTTPHosts ...string) (*Substrate, error) {
	if forceBoot {
		return nil, fmt.Errorf("SUBSTRATE_FORCE_BOOT is unsupported by substrate v0.5.0; configure snapshotConfig on the ActorTemplate instead")
	}
	if apiEndpoint == "" {
		return nil, fmt.Errorf("apiEndpoint required")
	}
	host, portStr, ok := strings.Cut(atenetRouterAddr, ":")
	if !ok {
		return nil, fmt.Errorf("atenetRouterAddr must be host:port, got %q", atenetRouterAddr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("atenetRouterAddr port: %w", err)
	}
	tns, tname, ok := strings.Cut(actorTemplateRef, "/")
	if !ok || tns == "" || tname == "" || strings.Contains(tname, "/") {
		return nil, fmt.Errorf("actorTemplateRef must be 'atespace/name', got %q", actorTemplateRef)
	}

	creds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	tokenFile := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	configured := os.Getenv("SUBSTRATE_TOKEN_FILE")
	if configured != "" {
		tokenFile = configured
	}
	if _, err := os.Stat(tokenFile); err == nil {
		opts = append(opts, grpc.WithPerRPCCredentials(serviceAccountTokenSource{path: tokenFile}))
	} else if configured != "" || !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat service account token: %w", err)
	}
	conn, err := grpc.NewClient(apiEndpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial ate-api-server: %w", err)
	}
	return &Substrate{
		control:           ateapipb.NewControlClient(conn),
		conn:              conn,
		actorTemplateNS:   tns,
		actorTemplateName: tname,
		atenetRouterHost:  host,
		atenetRouterPort:  port,
		allowedHTTPHosts:  append([]string(nil), allowedHTTPHosts...),
		probeClient: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 1 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				// Probe is a short HEAD — no pooling benefits, but disable
				// long-lived idle connections to atenet just to keep probe
				// traffic ephemeral.
				DisableKeepAlives: true,
			},
		},
	}, nil
}

// Close releases the gRPC connection.
func (s *Substrate) Close() { _ = s.conn.Close() }

func actorID(playwrightID string) string { return "pw-" + playwrightID }

func (s *Substrate) Ensure(ctx context.Context, playwrightID string) (Endpoint, error) {
	id := actorID(playwrightID)

	actor, err := s.getOrCreate(ctx, id)
	if err != nil {
		return Endpoint{}, err
	}
	if actor.GetActorTemplate().GetAtespace() != s.actorTemplateNS || actor.GetActorTemplate().GetName() != s.actorTemplateName {
		return Endpoint{}, fmt.Errorf("actor %s uses a different ActorTemplate; migrate or delete it explicitly", id)
	}
	if err := s.ensureEgressPolicy(ctx, id); err != nil {
		return Endpoint{}, err
	}

	// Drive to ACTOR_STATE_RUNNING before returning the atenet endpoint. Without
	// this, the proxy would forward the client's WS upgrade to atenet-router
	// for an actor that's still RESUMING from snapshot, and atenet would close
	// the upgrade (the client sees a spurious 403/connection drop). Hold the
	// request here instead so the client only sees latency.
	if _, err := s.waitForRunning(ctx, id, actor); err != nil {
		return Endpoint{}, err
	}

	// Running state alone does not establish application readiness on port 80.
	if err := s.waitUpstreamReady(ctx, id); err != nil {
		return Endpoint{}, err
	}

	// Substrate's atenet-router is the per-request data plane; the proxy hands
	// traffic to it with ate-target-actor: <atespace>/<id>.
	return Endpoint{Host: s.atenetRouterHost, Port: s.atenetRouterPort, TargetActor: s.actorTemplateNS + "/" + id}, nil
}

func (s *Substrate) actorRef(id string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: s.actorTemplateNS, Name: id}
}

// An explicit allowlist owns the actor's default policy. Never replace a
// conflicting operator-created policy or weaken the default-deny behavior.
func (s *Substrate) ensureEgressPolicy(ctx context.Context, id string) error {
	if len(s.allowedHTTPHosts) == 0 {
		return nil
	}
	rule := &ateapipb.EgressRule{Http: &ateapipb.HttpRule{Hostnames: s.allowedHTTPHosts, Ports: &ateapipb.Ports{Numbers: []int32{80}}}}
	request := &ateapipb.GetActorEgressPolicyRequest{Actor: s.actorRef(id), Name: "default"}
	policy, err := s.control.GetActorEgressPolicy(ctx, request)
	if status.Code(err) == codes.NotFound {
		policy, err = s.control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
			Actor: s.actorRef(id),
			EgressPolicy: &ateapipb.EgressPolicy{
				Metadata: &ateapipb.ResourceMetadata{Atespace: s.actorTemplateNS, Name: "default"},
				Rules:    []*ateapipb.EgressRule{rule},
			},
		})
		if status.Code(err) == codes.AlreadyExists {
			policy, err = s.control.GetActorEgressPolicy(ctx, request)
		}
	}
	if err != nil {
		return fmt.Errorf("egress policy for actor %s: %w", id, err)
	}
	if len(policy.GetRules()) != 1 || !proto.Equal(policy.GetRules()[0], rule) {
		return fmt.Errorf("actor %s default egress policy differs from SUBSTRATE_ALLOWED_HTTP_HOSTS; reconcile it explicitly", id)
	}
	return nil
}

type serviceAccountTokenSource struct{ path string }

func (s serviceAccountTokenSource) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	value := strings.TrimSpace(string(token))
	if value == "" {
		return nil, fmt.Errorf("service account token is empty")
	}
	return map[string]string{"authorization": "Bearer " + value}, nil
}

func (serviceAccountTokenSource) RequireTransportSecurity() bool { return true }

// waitUpstreamReady polls atenet-router until the upstream stops failing.
// atenet returns 503 with "remote connection failure" while the actor's
// Chromium hasn't bound to port 80 yet. Accept non-error application responses
// (including 404/405 for HEAD), but not authentication or actor-routing errors.
func (s *Substrate) waitUpstreamReady(ctx context.Context, id string) error {
	start := time.Now()
	deadline := start.Add(upstreamReadyDeadline)
	url := fmt.Sprintf("http://%s:%d/", s.atenetRouterHost, s.atenetRouterPort)
	attempt := 0
	for {
		attempt++
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, url, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build probe req: %w", err)
		}
		req.Header.Set("ate-target-actor", s.actorTemplateNS+"/"+id)
		resp, err := s.probeClient.Do(req)
		cancel()
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code >= 200 && code < 500 && code != http.StatusMisdirectedRequest && code != http.StatusUnauthorized && code != http.StatusForbidden {
				slog.Info("substrate: upstream ready",
					"id", id, "attempt", attempt,
					"elapsed", time.Since(start).String(), "code", code)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("substrate actor %s upstream not ready after %s (%d attempts)", id, upstreamReadyDeadline, attempt)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(upstreamProbeInterval):
		}
	}
}

// getOrCreate returns the current Actor or creates a fresh one on NotFound.
// Handles the race where a concurrent caller created the actor between our
// GetActor and CreateActor.
func (s *Substrate) getOrCreate(ctx context.Context, id string) (*ateapipb.Actor, error) {
	if resp, err := s.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: s.actorRef(id)}); err == nil {
		return resp, nil
	} else if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		return nil, fmt.Errorf("GetActor %s: %w", id, err)
	}
	cResp, err := s.control.CreateActor(ctx, &ateapipb.CreateActorRequest{
		Actor: &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: s.actorTemplateNS, Name: id},
			ActorTemplate: s.actorRef(s.actorTemplateName),
		},
	})
	if err == nil {
		return cResp, nil
	}
	if st, _ := status.FromError(err); st.Code() == codes.AlreadyExists {
		// Another caller beat us to CreateActor; re-fetch the actor.
		resp, err := s.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: s.actorRef(id)})
		if err != nil {
			return nil, fmt.Errorf("re-GetActor %s after AlreadyExists: %w", id, err)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("CreateActor %s: %w", id, err)
}

// waitForRunning polls until the actor is ACTOR_STATE_RUNNING. If the actor is
// SUSPENDED, we kick it via ResumeActor first. Returns the last observed
// Actor on success; on deadline, returns the last observed actor and an error
// for diagnosis; Ensure does not forward traffic on that failure.
func (s *Substrate) waitForRunning(ctx context.Context, id string, actor *ateapipb.Actor) (*ateapipb.Actor, error) {
	deadline := time.Now().Add(readyDeadline)
	resumed := false
	for {
		switch actor.GetStatus().GetState() {
		case ateapipb.ActorState_ACTOR_STATE_RUNNING:
			return actor, nil
		case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
			if !resumed {
				if _, err := s.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: s.actorRef(id)}); err != nil {
					st, _ := status.FromError(err)
					// FailedPrecondition can happen if the actor just left
					// SUSPENDED between our Get and Resume — fall through and
					// re-poll status.
					if st.Code() != codes.FailedPrecondition {
						return actor, fmt.Errorf("ResumeActor %s: %w", id, err)
					}
				}
				resumed = true
				slog.Info("substrate: resumed actor", "id", id)
			}
		case ateapipb.ActorState_ACTOR_STATE_CRASHED, ateapipb.ActorState_ACTOR_STATE_DELETING:
			return actor, fmt.Errorf("actor %s entered %s: %s", id, actor.GetStatus().GetState(), actor.GetStatus().GetCrash().GetMessage())
		default:
			// fall through to poll
		}
		if time.Now().After(deadline) {
			return actor, fmt.Errorf("actor %s did not reach ACTOR_STATE_RUNNING within %s (last=%s)",
				id, readyDeadline, actor.GetStatus())
		}
		select {
		case <-ctx.Done():
			return actor, ctx.Err()
		case <-time.After(readyPollInterval):
		}
		resp, err := s.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: s.actorRef(id)})
		if err != nil {
			return actor, fmt.Errorf("GetActor %s during wait: %w", id, err)
		}
		actor = resp
	}
}

func (s *Substrate) Delete(ctx context.Context, playwrightID string) error {
	id := actorID(playwrightID)
	// Substrate only deletes suspended actors. Suspend first; ignore the error
	// if it's already suspended.
	if _, err := s.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: s.actorRef(id)}); err != nil {
		st, _ := status.FromError(err)
		if st.Code() != codes.NotFound && st.Code() != codes.FailedPrecondition {
			return fmt.Errorf("SuspendActor %s: %w", id, err)
		}
	}
	if _, err := s.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: s.actorRef(id)}); err != nil {
		st, _ := status.FromError(err)
		if st.Code() != codes.NotFound {
			return fmt.Errorf("DeleteActor %s: %w", id, err)
		}
	}
	return nil
}

func (s *Substrate) List(ctx context.Context) ([]string, error) {
	var ids []string
	pageToken := ""
	for {
		resp, err := s.control.ListActors(ctx, &ateapipb.ListActorsRequest{
			Atespace: s.actorTemplateNS,
			Options:  &ateapipb.ListOptions{PageSize: 500, PageToken: pageToken},
		})
		if err != nil {
			return nil, fmt.Errorf("ListActors: %w", err)
		}
		for _, a := range resp.GetActors() {
			if a.GetActorTemplate().GetAtespace() != s.actorTemplateNS || a.GetActorTemplate().GetName() != s.actorTemplateName {
				continue
			}
			id := a.GetMetadata().GetName()
			if pid, ok := strings.CutPrefix(id, "pw-"); ok {
				ids = append(ids, pid)
			}
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		pageToken = resp.GetNextPageToken()
	}
	return ids, nil
}

// IsConflict is a small helper for callers that want to distinguish "already
// present" from real failures.
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.AlreadyExists
	}
	return errors.Is(err, errors.New("already exists"))
}
