// Package substrate is the kagent SandboxBackend that emits ate.dev/v1alpha1
// ActorTemplate resources, so SandboxAgent CRs are backed by agent-substrate
// instead of standard K8s Deployments.
//
// See SUBSTRATE.md at the repo root for the design and the Phase 0 validation
// that informed this implementation.
package substrate

import (
	"context"
	"fmt"
	"strings"

	substratev1 "github.com/agent-substrate/substrate/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Backend implements sandboxbackend.Backend by emitting ate.dev/v1alpha1
// ActorTemplate objects.
type Backend struct {
	cfg Config
}

var _ sandboxbackend.Backend = (*Backend)(nil)

// New returns a substrate sandbox backend configured from cfg. The caller is
// responsible for ensuring cfg has been validated; New does not panic on empty
// fields so the controller can start and surface configuration problems via
// the Agent's Ready condition rather than crash-looping.
func New(cfg Config) *Backend {
	return &Backend{cfg: cfg}
}

// ControlClient returns the configured Control client (or nil). Exposed so
// app-level wiring can share the dial with auxiliary components (e.g. the
// IdleSuspender) without re-dialing.
func (b *Backend) ControlClient() *ControlClient {
	if b == nil {
		return nil
	}
	return b.cfg.Control
}

// GetOwnedResourceTypes tells the kagent reconciler which kinds this backend
// produces, so the manager can set up watches.
func (b *Backend) GetOwnedResourceTypes() []client.Object {
	return []client.Object{
		&substratev1.ActorTemplate{},
	}
}

// OnDelete implements sandboxbackend.DeletingBackend. The reconciler calls
// this from a finalizer before letting Kubernetes garbage-collect the
// SandboxAgent, so we have a chance to suspend + delete the actor record in
// substrate's Valkey. Without this, actors orphan with stale last_snapshot
// references — recreating a SandboxAgent with the same name then resurfaces
// the wedged record (the failure mode we hit live in Phase A2).
//
// Idempotent. No-op when:
//   - the backend has no Control client wired up (operator opted into a
//     "no-lifecycle" deploy via empty --substrate-control-endpoint), or
//   - the actor doesn't exist in substrate (NotFound).
//
// Bounded by DeleteActorSequenced's internal 5-minute deadline; if substrate
// is wedged the reconciler will eventually time out and remove the finalizer
// anyway with a warning event (see the SandboxAgent reconciler).
func (b *Backend) OnDelete(ctx context.Context, nn types.NamespacedName) error {
	if b == nil || b.cfg.Control == nil {
		return nil
	}
	actorID := ActorIDFor(nn.Namespace, nn.Name)
	return b.cfg.Control.DeleteActorSequenced(ctx, actorID)
}

// BuildSandbox translates a kagent agent + PodTemplate into a substrate
// ActorTemplate.
//
// Translation is constrained by substrate's restricted container schema
// (ActorTemplate.spec.containers[] has: name, image, command, ports, env —
// NO volumes, init containers, security context, or probes). PodTemplate
// fields that don't map are dropped, surfaced via:
//
//   - the AnnotationUnsupportedFields annotation on the emitted ActorTemplate
//     (visible to users via `kubectl describe`), and
//   - a log line via the context's logger (visible in controller logs).
//
// Phase 2 only warns; the kagent translator should evolve to stop emitting
// these fields in sandbox mode (see SUBSTRATE.md §13).
func (b *Backend) BuildSandbox(ctx context.Context, in sandboxbackend.BuildInput) ([]client.Object, error) {
	if in.Agent == nil {
		return nil, fmt.Errorf("agent is required")
	}
	if len(in.PodTemplate.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod template has no containers")
	}

	name := in.Agent.GetName()
	if in.WorkloadName != "" {
		name = in.WorkloadName
	}

	labelUnion := mapsUnion(in.PodTemplate.Labels, in.ExtraLabels, in.Agent.GetLabels())

	annotations := map[string]string{}
	for k, v := range in.PodTemplate.Annotations {
		annotations[k] = v
	}
	unsupportedIDs, unsupportedSummary := detectUnsupported(in.PodTemplate)
	if len(unsupportedIDs) > 0 {
		annotations[AnnotationUnsupportedFields] = strings.Join(unsupportedIDs, ",")
		logf.FromContext(ctx).Info("substrate backend dropped unsupported PodTemplate fields",
			"agent", in.Agent.GetNamespace()+"/"+in.Agent.GetName(),
			"dropped", unsupportedIDs,
			"summary", unsupportedSummary,
		)
	}
	if len(annotations) == 0 {
		annotations = nil
	}

	primary := in.PodTemplate.Spec.Containers[0]
	container := translateContainer(primary)
	b.applyAgentImageOverride(&container, in)

	at := &substratev1.ActorTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: substratev1.GroupVersion.String(),
			Kind:       "ActorTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   in.Agent.GetNamespace(),
			Labels:      labelUnion,
			Annotations: annotations,
		},
		Spec: substratev1.ActorTemplateSpec{
			PauseImage: b.cfg.PauseImage,
			Containers: []substratev1.Container{
				container,
			},
			SnapshotsConfig: substratev1.SnapshotsConfig{
				Location: snapshotsLocation(b.cfg.SnapshotsLocation, in.Agent.GetNamespace(), name),
			},
			WorkerPoolRef: corev1.ObjectReference{
				Namespace: b.cfg.WorkerPoolNamespace,
				Name:      b.cfg.WorkerPoolName,
			},
			Runsc: b.runscConfig(),
		},
	}
	return []client.Object{at}, nil
}

// ComputeReady reflects the ActorTemplate's Phase into a Ready condition on
// the parent kagent (Sandbox)Agent.
//
// When the ActorTemplate is Ready AND a Control client is configured,
// ComputeReady also ensures the actor record exists in substrate so atenet
// can route traffic to it on the first request. This is the simplest place
// to hook actor lifecycle: the reconciler calls ComputeReady on every
// SandboxAgent reconcile, CreateActor is idempotent (AlreadyExists is
// treated as success), and the resulting actor status enriches the Ready
// message. Idle-suspend lives elsewhere (Phase 4).
//
// Phase semantics from substrate's actortemplate_controller.go:
//   - Initial / ResumeGoldenActor / WaitGoldenActor — the template is still
//     materializing its golden snapshot; Ready=False, no actors usable yet.
//   - Ready — golden snapshot exists; actors of this template can be created.
//   - Failed — terminal failure; Ready=False with reason.
func (b *Backend) ComputeReady(ctx context.Context, cl client.Client, nn types.NamespacedName) (metav1.ConditionStatus, string, string) {
	at := &substratev1.ActorTemplate{}
	if err := cl.Get(ctx, nn, at); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionUnknown, "ActorTemplateNotFound", err.Error()
		}
		return metav1.ConditionUnknown, "ActorTemplateGetFailed", err.Error()
	}
	switch at.Status.Phase {
	case substratev1.PhaseReady:
		// Side effect on Ready: make sure the substrate actor record exists.
		// Failing this is non-fatal for the Ready signal — we want operators
		// to still see "golden snapshot ready" even if the lifecycle hookup
		// is missing; but we surface the failure in the message.
		actorMsg := b.ensureActor(ctx, nn)
		msg := "golden snapshot ready"
		if actorMsg != "" {
			msg += "; " + actorMsg
		}
		return metav1.ConditionTrue, "ActorTemplateReady", msg
	case substratev1.PhaseFailed:
		return metav1.ConditionFalse, "ActorTemplateFailed", "substrate marked the ActorTemplate failed"
	case substratev1.PhaseInitial: // empty string ("") — substrate hasn't started reconciling yet
		return metav1.ConditionFalse, "ActorTemplatePending", "substrate has not started reconciling the ActorTemplate yet"
	default:
		// ResumeGoldenActor, WaitGoldenActor, or any future intermediate phase.
		return metav1.ConditionFalse, "ActorTemplate" + string(at.Status.Phase), fmt.Sprintf("substrate phase: %s", at.Status.Phase)
	}
}

// ensureActor idempotently creates the substrate actor record corresponding to
// a SandboxAgent. Returns a short status string (or "" when no client is
// configured / on failure that has been logged). Errors are intentionally
// soft so a hiccup talking to ate-api-server doesn't flap the SandboxAgent's
// Ready condition between True and Unknown.
func (b *Backend) ensureActor(ctx context.Context, nn types.NamespacedName) string {
	if b.cfg.Control == nil {
		return "" // No control wiring — operator is responsible for actor creation.
	}
	actorID := ActorIDFor(nn.Namespace, nn.Name)
	actor, err := b.cfg.Control.CreateActorIfMissing(ctx, actorID, nn.Namespace, nn.Name)
	if err != nil {
		logf.FromContext(ctx).Error(err, "substrate CreateActor failed", "actor", actorID)
		return fmt.Sprintf("actor %q: lifecycle error: %s", actorID, err.Error())
	}
	return fmt.Sprintf("actor %q is %s", actorID, actor.GetStatus())
}

// applyAgentImageOverride rewrites the container to use the substrate-shim
// kagent image when the operator has configured one. This is the path that
// makes "real kagent ADK agents on substrate" work: the shim image's
// entrypoint reads /config bytes out of env vars (because substrate has no
// volume mounts) and exec's kagent-adk static --local. When AgentImage is
// empty, we leave the translator's image+command alone — useful for BYO
// agents and stand-ins that don't need kagent config materialization.
//
// BYO agents are skipped unconditionally: they bring their own image +
// entrypoint and the translator doesn't produce a config.json for them, so
// swapping in the kagent shim image would leave KAGENT_CONFIG_JSON empty
// and the shim would exit at startup with code 64. That kills the gVisor
// sandbox before substrate's 20s golden-snapshot timer fires, and every
// subsequent runsc checkpoint/restore fails with "pause in state stopped".
func (b *Backend) applyAgentImageOverride(c *substratev1.Container, in sandboxbackend.BuildInput) {
	if b.cfg.AgentImage == "" {
		return
	}
	if in.Agent != nil {
		if spec := in.Agent.GetAgentSpec(); spec != nil && spec.Type == v1alpha2.AgentType_BYO {
			return
		}
	}
	c.Image = b.cfg.AgentImage
	// The shim ENTRYPOINT is /usr/local/bin/substrate-entrypoint.sh; pass
	// only the listener flags. The shim hardcodes `--local --filepath
	// /tmp/config` so we don't need to thread those through.
	//
	// Port 80 because atenet's ExtProc hardcodes the worker port (see
	// substrate cmd/servers/atenet/app/router/extproc.go).
	c.Command = []string{"/usr/local/bin/substrate-entrypoint.sh", "--host", "0.0.0.0", "--port", "80"}
	// Drop the translator's port=8080 in favor of port 80 so the published
	// agent card matches reality. Substrate ignores Ports for routing
	// (atenet hardcodes 80) but keeping it consistent helps `kubectl describe`.
	c.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 80, Protocol: corev1.ProtocolTCP}}
	// Inject the config bytes. The kagent translator already builds these
	// (they're what would normally go into the /config Secret); we pass
	// them through via BuildInput and inject as env vars here.
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "KAGENT_CONFIG_JSON", Value: in.ConfigJSON},
		corev1.EnvVar{Name: "KAGENT_AGENT_CARD_JSON", Value: in.AgentCardJSON},
	)
	if in.SRTSettingsJSON != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "KAGENT_SRT_SETTINGS_JSON", Value: in.SRTSettingsJSON})
	}
}

func (b *Backend) runscConfig() substratev1.RunscConfig {
	cfg := substratev1.RunscConfig{}
	if b.cfg.RunscAMD64URL != "" || b.cfg.RunscAMD64SHA256 != "" {
		cfg.AMD64 = &substratev1.RunscPlatformConfig{
			URL:        b.cfg.RunscAMD64URL,
			SHA256Hash: b.cfg.RunscAMD64SHA256,
		}
	}
	if b.cfg.RunscARM64URL != "" || b.cfg.RunscARM64SHA256 != "" {
		cfg.ARM64 = &substratev1.RunscPlatformConfig{
			URL:        b.cfg.RunscARM64URL,
			SHA256Hash: b.cfg.RunscARM64SHA256,
		}
	}
	return cfg
}

// snapshotsLocation builds a unique sub-prefix under the configured snapshots
// root, so two agents in different namespaces never collide.
func snapshotsLocation(root, ns, name string) string {
	if root == "" {
		return ""
	}
	root = strings.TrimRight(root, "/")
	return fmt.Sprintf("%s/%s/%s/", root, ns, name)
}

// mapsUnion merges any number of label maps; left-most map wins on conflict.
func mapsUnion(in ...map[string]string) map[string]string {
	total := 0
	for _, m := range in {
		total += len(m)
	}
	if total == 0 {
		return nil
	}
	out := make(map[string]string, total)
	for _, m := range in {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out
}
