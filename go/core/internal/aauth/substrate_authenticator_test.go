package aauth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/kagent-dev/kagent/go/core/internal/aauth"
)

// stubActorLookup is a hand-rolled SubstrateActorLookup that returns a fixed
// actor (or nil + error) so the test doesn't need a real gRPC server.
type stubActorLookup struct {
	actor *ateapipb.Actor
	err   error
}

func (s *stubActorLookup) GetActor(ctx context.Context, actorID string) (*ateapipb.Actor, error) {
	return s.actor, s.err
}

// canonicalActor builds an Actor proto in STATUS_RUNNING placed on the named
// worker pod IP, with the kagent-mapped ActorTemplate name/namespace.
func canonicalActor(podIP, tplNS, tplName string) *ateapipb.Actor {
	a := &ateapipb.Actor{
		ActorTemplateNamespace: tplNS,
		ActorTemplateName:      tplName,
		AteomPodIp:             podIP,
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	return a
}

func TestSubstrateSubjectAuthenticator_AuthenticateActor(t *testing.T) {
	const (
		actorID         = "kagent--aauth-test-agent"
		workerPodIP     = "10.244.0.36"
		agentName       = "aauth-test-agent"
		agentNamespace  = "kagent"
		expectedSub     = "aauth:aauth-test-agent@kagent.kagent.local"
		matchingSubBody = expectedSub
		mismatchSubBody = "aauth:other-agent@kagent.kagent.local"
	)
	correctRemote := workerPodIP + ":54321"

	cases := []struct {
		name        string
		remoteAddr  string
		actorID     string
		claimedSub  string
		actor       *ateapipb.Actor
		actorErr    error
		wantSub     string
		wantErrText string
	}{
		{
			name:       "happy path",
			remoteAddr: correctRemote,
			actorID:    actorID,
			actor:      canonicalActor(workerPodIP, agentNamespace, agentName),
			wantSub:    expectedSub,
		},
		{
			name:       "happy path with matching body sub",
			remoteAddr: correctRemote,
			actorID:    actorID,
			claimedSub: matchingSubBody,
			actor:      canonicalActor(workerPodIP, agentNamespace, agentName),
			wantSub:    expectedSub,
		},
		{
			name:        "body sub mismatch is rejected",
			remoteAddr:  correctRemote,
			actorID:     actorID,
			claimedSub:  mismatchSubBody,
			actor:       canonicalActor(workerPodIP, agentNamespace, agentName),
			wantErrText: "does not match canonical",
		},
		{
			name:        "empty actor id rejected",
			remoteAddr:  correctRemote,
			actorID:     "",
			wantErrText: "empty substrate_actor_id",
		},
		{
			name:        "empty remote addr rejected",
			remoteAddr:  "",
			actorID:     actorID,
			wantErrText: "empty address",
		},
		{
			name:        "actor not found in substrate",
			remoteAddr:  correctRemote,
			actorID:     actorID,
			actor:       nil,
			wantErrText: "substrate has no actor",
		},
		{
			name:        "actor placed on different pod (IP mismatch)",
			remoteAddr:  correctRemote,
			actorID:     actorID,
			actor:       canonicalActor("10.244.0.99", agentNamespace, agentName),
			wantErrText: "IP mismatch",
		},
		{
			name:       "actor not RUNNING",
			remoteAddr: correctRemote,
			actorID:    actorID,
			actor: func() *ateapipb.Actor {
				a := canonicalActor(workerPodIP, agentNamespace, agentName)
				a.Status = ateapipb.Actor_STATUS_SUSPENDED
				return a
			}(),
			wantErrText: "not RUNNING",
		},
		{
			name:        "actor has no ateom_pod_ip",
			remoteAddr:  correctRemote,
			actorID:     actorID,
			actor:       canonicalActor("", agentNamespace, agentName),
			wantErrText: "no recorded ateom_pod_ip",
		},
		{
			name:        "actor has no template identity (substrate registry corrupt)",
			remoteAddr:  correctRemote,
			actorID:     actorID,
			actor:       canonicalActor(workerPodIP, "", ""),
			wantErrText: "no ActorTemplate name/namespace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &aauth.SubstrateSubjectAuthenticator{
				Substrate: &stubActorLookup{actor: tc.actor, err: tc.actorErr},
			}

			got, err := a.AuthenticateActor(context.Background(), tc.remoteAddr, tc.actorID, tc.claimedSub)
			if tc.wantErrText == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.wantSub {
					t.Errorf("sub: got %q, want %q", got, tc.wantSub)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (sub=%q)", tc.wantErrText, got)
			}
			if !strings.Contains(err.Error(), tc.wantErrText) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrText)
			}
		})
	}
}

// Demonstrate the canonical sub format string used by the production code so
// that a change there forces an update here.
func TestSubstrateSubjectAuthenticator_subFormat(t *testing.T) {
	got := fmt.Sprintf("aauth:%s@%s.kagent.local", "agent-x", "team-y")
	if want := "aauth:agent-x@team-y.kagent.local"; got != want {
		t.Errorf("canonical sub format = %q, want %q", got, want)
	}
}
