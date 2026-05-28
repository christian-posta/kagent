package substrate

import (
	"context"
	"testing"

	substratev1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// validConfig is the minimum Config that produces a fully-populated
// ActorTemplate; tests assert against these constants below.
func validConfig() Config {
	return Config{
		WorkerPoolNamespace: "kagent-agents",
		WorkerPoolName:      "shared-pool",
		SnapshotsLocation:   "s3://kagent-snapshots",
		PauseImage:          "registry.k8s.io/pause:3.10.2",
		RunscAMD64URL:       "gs://gvisor/x86_64/runsc",
		RunscAMD64SHA256:    "deadbeef",
		RunscARM64URL:       "gs://gvisor/aarch64/runsc",
		RunscARM64SHA256:    "cafebabe",
	}
}

func TestBackend_BuildSandbox_emitsActorTemplate(t *testing.T) {
	b := New(validConfig())
	agent := &v1alpha2.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "team-a",
			Labels:    map[string]string{"team": "a"},
		},
	}
	pt := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "kagent"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "kagent",
				Image: "cr.kagent.dev/kagent-dev/kagent/app:dev",
				// Phase 0 lesson: substrate ignores image WORKDIR/ENTRYPOINT.
				// The translator MUST emit an absolute command here.
				Command: []string{"/usr/local/bin/uvicorn", "agent:app", "--app-dir", "/app"},
				Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
				Env: []corev1.EnvVar{
					{Name: "KAGENT_NAME", Value: "demo"},
					{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "openai-creds"},
							Key:                  "api-key",
						},
					}},
				},
			}},
		},
	}

	objs, err := b.BuildSandbox(context.Background(), sandboxbackend.BuildInput{
		Agent:       agent,
		PodTemplate: pt,
		ExtraLabels: map[string]string{"managed-by": "kagent"},
	})
	require.NoError(t, err)
	require.Len(t, objs, 1)

	at, ok := objs[0].(*substratev1.ActorTemplate)
	require.True(t, ok, "BuildSandbox must emit an ActorTemplate")

	// Identity
	require.Equal(t, "demo", at.Name)
	require.Equal(t, "team-a", at.Namespace)
	require.Equal(t, "ate.dev/v1alpha1", at.APIVersion)
	require.Equal(t, "ActorTemplate", at.Kind)

	// Backend-wide config copied through
	require.Equal(t, "registry.k8s.io/pause:3.10.2", at.Spec.PauseImage)
	require.Equal(t, "kagent-agents", at.Spec.WorkerPoolRef.Namespace)
	require.Equal(t, "shared-pool", at.Spec.WorkerPoolRef.Name)
	require.NotNil(t, at.Spec.Runsc.AMD64)
	require.Equal(t, "gs://gvisor/x86_64/runsc", at.Spec.Runsc.AMD64.URL)
	require.Equal(t, "deadbeef", at.Spec.Runsc.AMD64.SHA256Hash)

	// Per-agent snapshot path — namespaced so two agents can't collide.
	require.Equal(t, "s3://kagent-snapshots/team-a/demo/", at.Spec.SnapshotsConfig.Location)

	// Container translation: name, image, command, ports, env pass through.
	require.Len(t, at.Spec.Containers, 1)
	c := at.Spec.Containers[0]
	require.Equal(t, "kagent", c.Name)
	require.Equal(t, "cr.kagent.dev/kagent-dev/kagent/app:dev", c.Image)
	require.Equal(t, []string{"/usr/local/bin/uvicorn", "agent:app", "--app-dir", "/app"}, c.Command)
	require.Len(t, c.Ports, 1)
	require.Equal(t, int32(80), c.Ports[0].ContainerPort)

	// Env including ValueFrom must survive translation — substrate's container
	// schema accepts the full corev1.EnvVar.
	require.Len(t, c.Env, 2)
	require.Equal(t, "KAGENT_NAME", c.Env[0].Name)
	require.Equal(t, "OPENAI_API_KEY", c.Env[1].Name)
	require.NotNil(t, c.Env[1].ValueFrom)
	require.NotNil(t, c.Env[1].ValueFrom.SecretKeyRef)
	require.Equal(t, "openai-creds", c.Env[1].ValueFrom.SecretKeyRef.Name)

	// Label union: PodTemplate + ExtraLabels + Agent labels.
	require.Equal(t, "kagent", at.Labels["app"])
	require.Equal(t, "kagent", at.Labels["managed-by"])
	require.Equal(t, "a", at.Labels["team"])
}

// Phase 2: when the PodTemplate carries fields substrate can't represent
// (init containers, volume mounts, probes, etc.), BuildSandbox must surface
// what was dropped via the AnnotationUnsupportedFields annotation. Real
// SandboxAgents will carry many of these because kagent's translator still
// emits skills-init containers / config volumes today.
func TestBackend_BuildSandbox_recordsUnsupportedFields(t *testing.T) {
	b := New(validConfig())
	pt := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{},
			InitContainers:  []corev1.Container{{Name: "skills-init"}},
			Volumes:         []corev1.Volume{{Name: "config"}},
			Containers: []corev1.Container{{
				Name:    "kagent",
				Image:   "img",
				Command: []string{"/x"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/config"},
				},
				ReadinessProbe:  &corev1.Probe{},
				SecurityContext: &corev1.SecurityContext{},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{"cpu": resource.MustParse("1")},
				},
			}},
		},
	}
	objs, err := b.BuildSandbox(context.Background(), sandboxbackend.BuildInput{
		Agent:       &v1alpha2.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}},
		PodTemplate: pt,
	})
	require.NoError(t, err)
	require.Len(t, objs, 1)
	at := objs[0].(*substratev1.ActorTemplate)

	got := at.Annotations[AnnotationUnsupportedFields]
	require.NotEmpty(t, got, "annotation must be set when fields are dropped")
	// All categories present (order is detection order, not alphabetical).
	for _, want := range []string{
		"init-containers", "volumes", "pod-security-context",
		"volume-mounts", "probes", "container-security-context", "resources",
	} {
		require.Contains(t, got, want, "missing %q in %q", want, got)
	}
}

// And the inverse: clean PodTemplates leave no annotation behind.
func TestBackend_BuildSandbox_noAnnotationWhenClean(t *testing.T) {
	b := New(validConfig())
	objs, err := b.BuildSandbox(context.Background(), sandboxbackend.BuildInput{
		Agent: &v1alpha2.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}},
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "i", Command: []string{"/x"}}},
		}},
	})
	require.NoError(t, err)
	at := objs[0].(*substratev1.ActorTemplate)
	_, present := at.Annotations[AnnotationUnsupportedFields]
	require.False(t, present, "annotation should not be present on clean PodTemplate")
}

func TestBackend_BuildSandbox_rejectsEmptyAgent(t *testing.T) {
	b := New(validConfig())
	_, err := b.BuildSandbox(context.Background(), sandboxbackend.BuildInput{
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "i"}},
		}},
	})
	require.Error(t, err)
}

func TestBackend_BuildSandbox_rejectsEmptyContainers(t *testing.T) {
	b := New(validConfig())
	_, err := b.BuildSandbox(context.Background(), sandboxbackend.BuildInput{
		Agent:       &v1alpha2.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}},
		PodTemplate: corev1.PodTemplateSpec{},
	})
	require.Error(t, err)
}

func TestBackend_GetOwnedResourceTypes(t *testing.T) {
	b := New(validConfig())
	types := b.GetOwnedResourceTypes()
	require.Len(t, types, 1)
	_, ok := types[0].(*substratev1.ActorTemplate)
	require.True(t, ok, "owned resource type must be ActorTemplate")
}

// ComputeReady reads ActorTemplate.Status.Phase and translates it into a
// Ready condition. Each phase has a defined contract — exercise the meaningful
// ones via a fake client.
func TestBackend_ComputeReady(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, substratev1.AddToScheme(scheme))

	cases := []struct {
		name       string
		phase      substratev1.PhaseType
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"ready", substratev1.PhaseReady, metav1.ConditionTrue, "ActorTemplateReady"},
		{"failed", substratev1.PhaseFailed, metav1.ConditionFalse, "ActorTemplateFailed"},
		{"initial empty", substratev1.PhaseInitial, metav1.ConditionFalse, "ActorTemplatePending"},
		{"resume golden", substratev1.PhaseResumeGoldenActor, metav1.ConditionFalse, "ActorTemplateResumeGoldenActor"},
		{"wait golden", substratev1.PhaseWaitGoldenActor, metav1.ConditionFalse, "ActorTemplateWaitGoldenActor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := &substratev1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
				Status:     substratev1.ActorTemplateStatus{Phase: tc.phase},
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(at).WithStatusSubresource(at).Build()

			b := New(validConfig())
			status, reason, _ := b.ComputeReady(context.Background(), cl, types.NamespacedName{Namespace: "team-a", Name: "demo"})
			require.Equal(t, tc.wantStatus, status)
			require.Equal(t, tc.wantReason, reason)
		})
	}
}

func TestBackend_ComputeReady_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, substratev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	b := New(validConfig())
	status, reason, _ := b.ComputeReady(context.Background(), cl, types.NamespacedName{Namespace: "x", Name: "missing"})
	require.Equal(t, metav1.ConditionUnknown, status)
	require.Equal(t, "ActorTemplateNotFound", reason)
}

// Make sure the substrate scheme is actually buildable from outside the package
// (sanity: we depend on the substrate module's exported AddToScheme).
func TestSubstrateSchemeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, substratev1.AddToScheme(scheme))
	gvk := schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "ActorTemplate"}
	obj, err := scheme.New(gvk)
	require.NoError(t, err)
	require.IsType(t, &substratev1.ActorTemplate{}, obj)
}

