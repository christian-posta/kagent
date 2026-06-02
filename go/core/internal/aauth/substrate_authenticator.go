// Package aauth — substrate-flavored authenticator.
//
// Declarative kagent agents hosted on agent-substrate cannot use the regular
// TokenReview path: substrate multiplexes many actors onto a small pool of
// worker pods, and its restricted container schema doesn't allow mounting a
// projected SA token volume into the actor's gVisor sandbox. So the actor
// has no K8s bearer token to present.
//
// Instead the substrate-aware authenticator does a single attested lookup:
//
//	Control.GetActor(actor_id) returns {ateom_pod_ip, actor_template_name,
//	actor_template_namespace, status} — everything we need.
//
// Two checks gate the mint:
//   - Source IP: the request's TCP source IP must match actor.ateom_pod_ip.
//     K8s pod networking preserves source IP for cluster-internal traffic,
//     so this binds the request to the worker pod hosting the actor.
//   - Running status: only RUNNING actors can mint.
//
// The canonical aa-agent+jwt sub claim is derived directly from substrate's
// view of the ActorTemplate name/namespace, which the kagent translator sets
// 1:1 with the Agent CR's name/namespace at provision time. No separate
// label lookup is needed — substrate is the source of truth for placement
// AND for the agent identity it's been told to host.
//
// Residual gap: actors multiplexed on the same worker can mint as each other
// if they escape gVisor. gVisor's sandbox boundary — not AAuth — is the
// actual isolation barrier inside a worker. See demo/substrate-poc/
// AAUTH-PLAN.md.
package aauth

import (
	"context"
	"fmt"
	"net"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// SubstrateActorLookup is the slice of substrate.ControlClient this
// authenticator needs. Kept as an interface so unit tests don't have to dial
// a real gRPC server.
type SubstrateActorLookup interface {
	GetActor(ctx context.Context, actorID string) (*ateapipb.Actor, error)
}

// SubstrateSubjectAuthenticator implements the substrate-mode mint flow.
//
// Unlike K8sSubjectAuthenticator (which uses the bearer's SA username as the
// identity), this one uses substrate's Control.GetActor to attest both
// placement (worker pod IP) AND identity (template name/namespace). No
// bearer token is needed — the actor's restricted gVisor sandbox has no SA
// token to present.
type SubstrateSubjectAuthenticator struct {
	// Substrate is the substrate Control client used to look up actor →
	// worker placement and to read the actor's template identity.
	Substrate SubstrateActorLookup
}

// AuthenticateActor runs the verification chain and returns the canonical
// aa-agent+jwt sub claim on success.
//
// remoteAddr is the request's source address (host:port form, as in
// http.Request.RemoteAddr). claimedSub is the optional `sub` field from the
// mint request body; when non-empty it must match the canonical sub derived
// from substrate's view of the actor's template.
func (s *SubstrateSubjectAuthenticator) AuthenticateActor(ctx context.Context, remoteAddr, actorID, claimedSub string) (string, error) {
	if actorID == "" {
		return "", fmt.Errorf("empty substrate_actor_id")
	}
	callerIP, err := hostFromAddr(remoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid remote address %q: %w", remoteAddr, err)
	}

	actor, err := s.Substrate.GetActor(ctx, actorID)
	if err != nil {
		return "", fmt.Errorf("substrate GetActor %q: %w", actorID, err)
	}
	if actor == nil {
		return "", fmt.Errorf("substrate has no actor %q", actorID)
	}
	if actor.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
		return "", fmt.Errorf("actor %q is %s (not RUNNING)", actorID, actor.GetStatus())
	}
	if actor.GetAteomPodIp() == "" {
		return "", fmt.Errorf("actor %q has no recorded ateom_pod_ip in substrate", actorID)
	}
	if actor.GetAteomPodIp() != callerIP {
		return "", fmt.Errorf(
			"IP mismatch: request came from %s but actor %q is hosted on %s",
			callerIP, actorID, actor.GetAteomPodIp(),
		)
	}

	// Derive canonical sub directly from substrate's view of the
	// ActorTemplate. The kagent translator sets template.Name = agent.Name
	// and template.Namespace = agent.Namespace at provision time (see
	// substrate.BuildSandbox), so what substrate returns IS the agent
	// identity.
	agentName := actor.GetActorTemplateName()
	agentNamespace := actor.GetActorTemplateNamespace()
	if agentName == "" || agentNamespace == "" {
		return "", fmt.Errorf(
			"actor %q has no ActorTemplate name/namespace in substrate (got %q/%q)",
			actorID, agentNamespace, agentName,
		)
	}
	canonical := fmt.Sprintf("aauth:%s@%s.kagent.local", agentName, agentNamespace)
	if claimedSub != "" && claimedSub != canonical {
		return "", fmt.Errorf("body sub %q does not match canonical %q", claimedSub, canonical)
	}
	return canonical, nil
}

// hostFromAddr extracts the IP from a host:port address. Accepts plain IPs
// too (used in tests where there's no port). Returns an error only for
// totally unparseable input.
func hostFromAddr(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("empty address")
	}
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host, nil
	}
	// No port — try to validate the input is still an IP.
	if ip := net.ParseIP(addr); ip != nil {
		return addr, nil
	}
	return "", err
}
