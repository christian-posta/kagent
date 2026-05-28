package substrate

import (
	"net/http"
	"strings"

	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
)

// NewSandboxRoutingFunc builds the sandbox-routing hook the A2A registrar
// uses to send traffic for sandbox-mode agents through substrate's
// atenet-router.
//
// routerURL is the dial target — typically "http://atenet-router.ate-system.svc"
// in-cluster, or a port-forward like "http://localhost:18000" for local dev.
// When routerURL is empty, this returns nil and the registrar falls back to
// the default "dial the per-agent Service" path. (Useful for migration paths
// where the substrate backend is configured but the router isn't wired up
// yet, or for unit tests.)
//
// idleSuspender is optional. When non-nil, every outbound request feeds
// IdleSuspender.Touch so the sweep loop can later auto-suspend idle actors.
// Passing nil keeps actors RUNNING until they're suspended by hand.
//
// For each request the returned function:
//   - returns the routerURL as the dial target, so the A2AClient connects to
//     atenet rather than a per-agent ClusterIP;
//   - returns an *http.Client whose RoundTripper rewrites the outgoing Host
//     header to "<actor-id>.actors.resources.substrate.ate.dev", which is
//     what atenet's ExtProc parses to extract the actor identity.
func NewSandboxRoutingFunc(routerURL string, idleSuspender *IdleSuspender) sandboxbackend.SandboxRoutingFunc {
	if strings.TrimSpace(routerURL) == "" {
		return nil
	}
	var onRequest func(actorID string)
	if idleSuspender != nil {
		onRequest = idleSuspender.Touch
	}
	return func(namespace, name string) (string, *http.Client) {
		actorID := ActorIDFor(namespace, name)
		client := &http.Client{
			Transport: HostRewritingTransport(actorID, http.DefaultTransport, onRequest),
		}
		return routerURL, client
	}
}
