package substrate

import (
	"fmt"
	"strings"

	substratev1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// AnnotationUnsupportedFields is set on emitted ActorTemplate objects whenever
// the source PodTemplate carried fields that substrate's restricted container
// schema cannot represent (init containers, volume mounts, probes, etc.). The
// value is a human-readable, comma-separated summary; `kubectl describe` and
// controller logs both expose it. Phase 2 surfaces these as warnings only —
// rejecting them is deferred until kagent's substrate-aware translator path
// stops emitting them in the first place.
const AnnotationUnsupportedFields = "kagent.dev/substrate-unsupported"

// detectUnsupported scans the PodTemplate for kagent-translator outputs that
// substrate can't carry. It returns a deterministic, sorted-by-discovery list
// of short identifiers and a human-readable summary string.
//
// Categories are deliberately coarse so the annotation stays readable; details
// belong in controller logs (which BuildSandbox writes separately).
func detectUnsupported(pt corev1.PodTemplateSpec) (ids []string, summary string) {
	add := func(id string) {
		for _, x := range ids {
			if x == id {
				return
			}
		}
		ids = append(ids, id)
	}

	if len(pt.Spec.InitContainers) > 0 {
		add("init-containers")
	}
	if len(pt.Spec.Volumes) > 0 {
		add("volumes")
	}
	if pt.Spec.SecurityContext != nil {
		add("pod-security-context")
	}
	if len(pt.Spec.Containers) > 0 {
		c := pt.Spec.Containers[0]
		if len(c.VolumeMounts) > 0 {
			add("volume-mounts")
		}
		if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
			add("probes")
		}
		if c.SecurityContext != nil {
			add("container-security-context")
		}
		if c.Resources.Requests != nil || c.Resources.Limits != nil {
			// substrate's Container has no Resources field; pool-level workers
			// share resource limits. Worth flagging because users will expect
			// per-agent requests/limits to apply.
			add("resources")
		}
	}

	if len(ids) == 0 {
		return nil, ""
	}
	summary = fmt.Sprintf("substrate dropped: %s (see SUBSTRATE.md §11 for why)", strings.Join(ids, ", "))
	return ids, summary
}

// translateContainer maps the primary container of a kagent PodTemplateSpec
// onto substrate's restricted Container schema.
//
// Lossy on purpose: substrate's Container has only name/image/command/ports/env.
// Anything else on the kagent container (volume mounts, security context,
// probes, resources) is dropped here.
//
// Phase 0 surfaced two non-obvious requirements:
//
//  1. substrate sets Cwd:/ and does NOT honor the image WORKDIR/ENTRYPOINT/CMD,
//     so Command must be a fully-qualified absolute path. The kagent translator
//     is responsible for ensuring this when WorkloadMode==sandbox; we don't
//     synthesize a command here.
//  2. atenet hardcodes the worker target port to 80. The kagent translator
//     should already be setting port 80 on the primary container in sandbox
//     mode; we surface what's there without rewriting it.
func translateContainer(src corev1.Container) substratev1.Container {
	// Substrate's Container has only `Command` (no `Args`). Kagent's translator
	// follows the kube convention of putting the entrypoint in `Command` and
	// extra arguments in `Args`. To preserve invocation semantics we have to
	// concatenate. Discovered live in Phase 2 against a BYO SandboxAgent:
	// without this, only the entrypoint was being passed to substrate and
	// uvicorn started with no module to load.
	combined := make([]string, 0, len(src.Command)+len(src.Args))
	combined = append(combined, src.Command...)
	combined = append(combined, src.Args...)

	out := substratev1.Container{
		Name:    nonEmpty(src.Name, "agent"),
		Image:   src.Image,
		Command: combined,
		Ports:   copyPorts(src.Ports),
		Env:     copyEnv(src.Env),
	}
	return out
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func copyPorts(in []corev1.ContainerPort) []corev1.ContainerPort {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.ContainerPort, len(in))
	copy(out, in)
	return out
}

// copyEnv preserves both Value and ValueFrom (SecretKeyRef / FieldRef / etc.).
// Verified in Phase 0 that substrate happily passes corev1.EnvVar through to
// the sandboxed container, including secret references — provided the secret
// exists in the worker pod's namespace.
func copyEnv(in []corev1.EnvVar) []corev1.EnvVar {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, len(in))
	copy(out, in)
	return out
}
