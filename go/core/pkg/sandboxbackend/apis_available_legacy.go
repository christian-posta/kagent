package sandboxbackend

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsandboxv1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// ensureAgentSandboxLegacy is the original (pre-substrate) implementation of
// EnsureAgentSandboxAPIsRegistered, retained so the deprecated public function
// keeps its old behavior.
func ensureAgentSandboxLegacy(ctx context.Context, c client.Client) error {
	checks := []struct {
		list client.ObjectList
		kind string
	}{
		{&agentsandboxv1.SandboxList{}, "Sandbox (agents.x-k8s.io/v1alpha1)"},
	}
	for _, ch := range checks {
		if err := c.List(ctx, ch.list, client.Limit(1)); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("agent-sandbox API %s is not available on this cluster; install the agent-sandbox CRDs and controller before using SandboxAgent: %w", ch.kind, err)
			}
			return fmt.Errorf("could not reach agent-sandbox API %s (check RBAC and apiserver connectivity): %w", ch.kind, err)
		}
	}
	return nil
}
