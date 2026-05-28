package sandboxbackend

import (
	"context"

	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BuildInput carries the pod template for a Sandbox workload (agents.x-k8s.io Sandbox).
type BuildInput struct {
	Agent        v1alpha2.AgentObject
	PodTemplate  corev1.PodTemplateSpec
	WorkloadName string
	ExtraLabels  map[string]string

	// ConfigJSON, AgentCardJSON, SRTSettingsJSON carry the raw bytes the
	// kagent translator would otherwise stuff into the /config Secret
	// mounted at /config on the agent pod. Backends whose target runtime
	// can't mount Secrets (e.g. substrate, whose ActorTemplate.spec.containers
	// schema has no volumes/volumeMounts) can ingest these and either:
	//   - inline them into env vars + run a config-from-env shim entrypoint
	//     (this is what the substrate backend does), or
	//   - fetch them from the controller at startup, etc.
	//
	// agentsxk8s/openshell backends ignore these — their target Sandbox
	// types accept full PodSpecs and keep the Secret-mounted-at-/config path.
	ConfigJSON      string
	AgentCardJSON   string
	SRTSettingsJSON string
}

// Backend builds sandbox CRD objects and evaluates their readiness.
type Backend interface {
	BuildSandbox(ctx context.Context, in BuildInput) ([]client.Object, error)
	GetOwnedResourceTypes() []client.Object

	// ComputeReady reflects implementation-specific status into condition pieces for Agent.status.
	ComputeReady(ctx context.Context, cl client.Client, nn types.NamespacedName) (status metav1.ConditionStatus, reason, message string)
}

// DeletingBackend is the optional capability for a backend that needs to do
// work BEFORE the Kubernetes garbage collector removes the SandboxAgent
// resource. The reconciler installs a finalizer on SandboxAgents when the
// active backend implements this interface; on a delete it calls OnDelete
// then removes the finalizer.
//
// The substrate backend implements this to call DeleteActorSequenced on
// the substrate Control API — otherwise the actor record orphans in Valkey
// with a stale last_snapshot reference (the wedged-actor failure mode we
// hit live in Phase A2). Default backends (agentsxk8s, openshell) don't
// need this — their resources get cleaned up by Kubernetes ownerReference
// cascade.
//
// OnDelete should be idempotent and bounded in time — the reconciler will
// remove the finalizer regardless after its own deadline elapses, so a
// permanently-unreachable backend can't block CR deletion.
type DeletingBackend interface {
	OnDelete(ctx context.Context, nn types.NamespacedName) error
}
