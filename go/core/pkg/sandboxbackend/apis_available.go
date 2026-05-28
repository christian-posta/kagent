package sandboxbackend

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureSandboxBackendAPIsRegistered checks that every Kubernetes API the
// configured sandbox backend produces is actually installed on this cluster.
// Different backends (agent-sandbox, agent-substrate, …) produce different
// CRDs; instead of hard-coding the agent-sandbox kinds, we ask the backend
// which kinds it owns and probe each one.
//
// When a CRD is missing, the apiserver returns a *meta.NoKindMatchError, which
// surfaces here as a clear prerequisite error instead of a late reconcile
// failure during BuildSandbox.
//
// Call this before creating or reconciling SandboxAgent when a sandbox backend
// is configured.
func EnsureSandboxBackendAPIsRegistered(ctx context.Context, c client.Client, backend Backend) error {
	if backend == nil {
		return nil
	}
	scheme := c.Scheme()
	for _, obj := range backend.GetOwnedResourceTypes() {
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil || len(gvks) == 0 {
			// Type isn't registered in the scheme at all — not a cluster-side
			// problem to report here; whoever wired the backend should have
			// added AddToScheme. Skip rather than fail noisily.
			continue
		}
		gvk := gvks[0]
		listGVK := schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"}
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(listGVK)
		if err := c.List(ctx, ul, client.Limit(1)); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("sandbox backend requires %s but the CRD is not installed on this cluster: %w", gvk.GroupKind().String(), err)
			}
			return fmt.Errorf("could not list %s (check RBAC and apiserver connectivity): %w", gvk.GroupKind().String(), err)
		}
	}
	return nil
}

// EnsureAgentSandboxAPIsRegistered is kept as a thin wrapper so external
// callers don't have to change. New code should call
// EnsureSandboxBackendAPIsRegistered with the backend directly.
//
// Deprecated: use EnsureSandboxBackendAPIsRegistered.
func EnsureAgentSandboxAPIsRegistered(ctx context.Context, c client.Client) error {
	// We can't know the backend from this signature; fall back to the legacy
	// behavior of unconditionally checking the agent-sandbox API. Anyone who
	// still imports this helper gets the old behavior. The reconciler has
	// been migrated to the new entry point.
	return ensureAgentSandboxLegacy(ctx, c)
}
