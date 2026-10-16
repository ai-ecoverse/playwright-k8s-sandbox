package backend

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/carlossg/playwright-k8s-sandbox/internal/backend/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type controlStub struct {
	ateapipb.ControlClient
	getActor     func(*ateapipb.GetActorRequest) (*ateapipb.Actor, error)
	createActor  func(*ateapipb.CreateActorRequest) (*ateapipb.Actor, error)
	resumeActor  func(*ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error)
	getPolicy    func(*ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error)
	createPolicy func(*ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error)
	listActors   func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error)
}

func (c controlStub) GetActor(_ context.Context, r *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	return c.getActor(r)
}
func (c controlStub) CreateActor(_ context.Context, r *ateapipb.CreateActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	return c.createActor(r)
}
func (c controlStub) ResumeActor(_ context.Context, r *ateapipb.ResumeActorRequest, _ ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	return c.resumeActor(r)
}
func (c controlStub) GetActorEgressPolicy(_ context.Context, r *ateapipb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	return c.getPolicy(r)
}
func (c controlStub) CreateActorEgressPolicy(_ context.Context, r *ateapipb.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	return c.createPolicy(r)
}
func (c controlStub) ListActors(_ context.Context, r *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	return c.listActors(r)
}

func TestSubstrateEnsureV050(t *testing.T) {
	actor := &ateapipb.Actor{
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "test", Name: "browser"},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}
	gets, resumes := 0, 0
	s := &Substrate{actorTemplateNS: "test", actorTemplateName: "browser"}
	s.control = controlStub{
		getActor: func(r *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
			if r.Actor.Atespace != "test" || r.Actor.Name != "pw-alpha" {
				t.Fatalf("wrong actor ref: %v", r)
			}
			gets++
			if gets == 1 {
				return nil, status.Error(codes.NotFound, "missing")
			}
			return actor, nil
		},
		createActor: func(r *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
			if r.Actor.Metadata.Atespace != "test" || r.Actor.Metadata.Name != "pw-alpha" || r.Actor.ActorTemplate.Name != "browser" {
				t.Fatalf("wrong create: %v", r)
			}
			return nil, status.Error(codes.AlreadyExists, "racing create")
		},
		resumeActor: func(r *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
			resumes++
			actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
			return &ateapipb.ResumeActorResponse{}, nil
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.Header.Get("ate-target-actor") != "test/pw-alpha" {
			t.Errorf("bad readiness probe: %v", r)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s.atenetRouterHost, _, _ = net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	s.atenetRouterPort, _ = strconv.Atoi(port)
	s.probeClient = server.Client()
	ep, err := s.Ensure(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if ep.TargetActor != "test/pw-alpha" || resumes != 1 || gets != 3 {
		t.Fatalf("endpoint=%v resumes=%d gets=%d", ep, resumes, gets)
	}
}

func TestSubstrateEgressPolicy(t *testing.T) {
	s := &Substrate{actorTemplateNS: "test"}
	if err := s.ensureEgressPolicy(t.Context(), "pw-alpha"); err != nil {
		t.Fatal(err)
	}
	s.allowedHTTPHosts = []string{"fixture.test"}
	var saved *ateapipb.EgressPolicy
	s.control = controlStub{
		getPolicy: func(r *ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
			if r.Actor.Atespace != "test" || r.Name != "default" {
				t.Fatalf("wrong scope: %v", r)
			}
			if saved == nil {
				return nil, status.Error(codes.NotFound, "missing")
			}
			return saved, nil
		},
		createPolicy: func(r *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
			saved = r.EgressPolicy
			return nil, status.Error(codes.AlreadyExists, "racing create")
		},
	}
	if err := s.ensureEgressPolicy(t.Context(), "pw-alpha"); err != nil {
		t.Fatal(err)
	}
	if saved.Rules[0].Http.Ports.Numbers[0] != 80 {
		t.Fatal("HTTP must be limited to port 80")
	}
	if err := s.ensureEgressPolicy(t.Context(), "pw-alpha"); err != nil {
		t.Fatal(err)
	}
	saved.Rules[0].Http.Hostnames = []string{"different.test"}
	if err := s.ensureEgressPolicy(t.Context(), "pw-alpha"); err == nil {
		t.Fatal("conflicting policy accepted")
	}
	s.control = controlStub{getPolicy: func(*ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
		return nil, status.Error(codes.PermissionDenied, "denied")
	}}
	if err := s.ensureEgressPolicy(t.Context(), "pw-alpha"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("policy error swallowed: %v", err)
	}
}

func TestSubstrateListPagination(t *testing.T) {
	s := &Substrate{actorTemplateNS: "test", actorTemplateName: "browser"}
	calls := 0
	s.control = controlStub{listActors: func(r *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		if r.Atespace != "test" {
			t.Fatalf("unscoped list: %v", r)
		}
		calls++
		if calls == 1 {
			if r.Options.PageToken != "" {
				t.Fatal("unexpected initial token")
			}
			return &ateapipb.ListActorsResponse{NextPageToken: "next"}, nil
		}
		if r.Options.PageToken != "next" {
			t.Fatal("pagination lost")
		}
		return &ateapipb.ListActorsResponse{Actors: []*ateapipb.Actor{
			{Metadata: &ateapipb.ResourceMetadata{Name: "pw-alpha"}, ActorTemplate: &ateapipb.ObjectRef{Atespace: "test", Name: "browser"}},
			{Metadata: &ateapipb.ResourceMetadata{Name: "pw-other"}, ActorTemplate: &ateapipb.ObjectRef{Atespace: "elsewhere", Name: "browser"}},
		}}, nil
	}}
	ids, err := s.List(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != "alpha" || calls != 2 {
		t.Fatalf("ids=%v calls=%d error=%v", ids, calls, err)
	}
}

func TestSubstrateTerminalStateAndCancellation(t *testing.T) {
	s := &Substrate{}
	if _, err := s.waitForRunning(t.Context(), "pw-alpha", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED}}); err == nil {
		t.Fatal("crashed actor accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.waitForRunning(ctx, "pw-alpha", &ateapipb.Actor{}); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := NewSubstrateWithOptions("unused", "router:80", "test/browser", true); err == nil {
		t.Fatal("legacy force boot accepted")
	}
}

func TestServiceAccountTokenRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	source := serviceAccountTokenSource{path: path}
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		headers, err := source.GetRequestMetadata(t.Context())
		if err != nil || headers["authorization"] != "Bearer "+token {
			t.Fatalf("headers=%v err=%v", headers, err)
		}
	}
	if err := os.WriteFile(path, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.GetRequestMetadata(t.Context()); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := source.GetRequestMetadata(t.Context()); err == nil {
		t.Fatal("missing token accepted")
	}
	if !source.RequireTransportSecurity() {
		t.Fatal("bearer token must require TLS")
	}
}

func TestSubstrateExplicitMissingToken(t *testing.T) {
	t.Setenv("SUBSTRATE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := NewSubstrate("api:443", "router:80", "test/browser"); err == nil {
		t.Fatal("explicitly configured missing token file accepted")
	}
}
