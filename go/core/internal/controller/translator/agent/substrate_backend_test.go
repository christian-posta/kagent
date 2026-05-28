package agent_test

import (
	"context"
	"testing"

	substratev1 "github.com/agent-substrate/substrate/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	translator "github.com/kagent-dev/kagent/go/core/internal/controller/translator/agent"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	schemev1 "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Phase 2 integration test: prove the kagent translator + substrate backend
// emit a real ActorTemplate end-to-end (not just unit-tested in isolation).
// After the SandboxAgent unification (SUBSTRATE.md §21) this uses
// kind: Agent with spec.workloadMode=sandbox.
//
// We use a BYO sandbox-mode Agent deliberately:
//   - The Declarative path would require a ModelConfig and pull in a lot of
//     kagent-internal config-building machinery that's orthogonal to what we
//     want to test (which is: does the substrate-backed sandbox-mode workflow
//     produce a correctly-shaped ActorTemplate?).
//   - BYO is the path real users would adopt first if they wanted to put a
//     pre-built agent image inside substrate.
func Test_AdkApiTranslator_SandboxModeAgent_BYO_EmitsActorTemplate(t *testing.T) {
	ctx := context.Background()
	scheme := schemev1.Scheme
	require.NoError(t, v1alpha2.AddToScheme(scheme))
	require.NoError(t, substratev1.AddToScheme(scheme))

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-ns"}}
	cmd := "/usr/local/bin/uvicorn"
	sandboxMode := v1alpha2.WorkloadModeSandbox
	sa := &v1alpha2.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-sub", Namespace: "sandbox-ns"},
		Spec: v1alpha2.AgentSpec{
			Type:         v1alpha2.AgentType_BYO,
			WorkloadMode: &sandboxMode,
			BYO: &v1alpha2.BYOAgentSpec{
				Deployment: &v1alpha2.ByoDeploymentSpec{
					Image: "localhost:5001/substrate-poc-agent:p0",
					Cmd:   &cmd,
					Args:  []string{"agent:app", "--app-dir", "/app", "--host", "0.0.0.0", "--port", "80"},
				},
			},
		},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ns).
		Build()

	sbBackend := substrate.New(substrate.Config{
		WorkerPoolNamespace: "kagent-substrate-poc",
		WorkerPoolName:      "poc-pool",
		SnapshotsLocation:   "gs://ate-snapshots",
		PauseImage:          "registry.k8s.io/pause:3.10.2",
		RunscAMD64URL:       "gs://gvisor/x86_64/runsc",
		RunscAMD64SHA256:    "deadbeef",
	})

	trans := translator.NewAdkApiTranslator(
		kubeClient,
		types.NamespacedName{Namespace: "sandbox-ns", Name: "default"},
		nil,
		"",
		sbBackend,
	)
	outputs, err := translator.TranslateAgent(ctx, trans, sa)
	require.NoError(t, err)
	require.NotNil(t, outputs)

	// Inventory the produced objects. With substrate as the sandbox backend:
	//   - one ActorTemplate (the substrate workload)
	//   - NO Deployment, NO Service (those belong to the deployment-mode path)
	//   - any number of supporting objects (Secret/SA) — we don't assert on them
	var (
		at         *substratev1.ActorTemplate
		sawDeploy  bool
		sawService bool
	)
	for _, o := range outputs.Manifest {
		switch v := o.(type) {
		case *substratev1.ActorTemplate:
			at = v
		case *appsv1.Deployment:
			sawDeploy = true
		case *corev1.Service:
			sawService = true
		}
	}
	require.NotNil(t, at, "translator must emit an ActorTemplate when sandbox backend is substrate")
	require.False(t, sawDeploy, "sandbox runtime must not include a Deployment")
	require.False(t, sawService, "sandbox runtime must not include a Service")

	// Identity + ownership: the kagent reconciler should have set the
	// ActorTemplate's controller reference to the Agent. (Verified upstream
	// at manifest_builder.go — we just confirm it happened here.)
	require.Equal(t, "byo-sub", at.Name)
	require.Equal(t, "sandbox-ns", at.Namespace)
	require.NotEmpty(t, at.OwnerReferences, "ActorTemplate must have a controller ref to the Agent")
	require.Equal(t, "Agent", at.OwnerReferences[0].Kind)
	require.Equal(t, "byo-sub", at.OwnerReferences[0].Name)

	// Substrate-side spec carries the configured backend values.
	require.Equal(t, "kagent-substrate-poc", at.Spec.WorkerPoolRef.Namespace)
	require.Equal(t, "poc-pool", at.Spec.WorkerPoolRef.Name)
	require.Equal(t, "registry.k8s.io/pause:3.10.2", at.Spec.PauseImage)
	require.Equal(t, "gs://ate-snapshots/sandbox-ns/byo-sub/", at.Spec.SnapshotsConfig.Location)
	require.NotNil(t, at.Spec.Runsc.AMD64)
	require.Equal(t, "deadbeef", at.Spec.Runsc.AMD64.SHA256Hash)

	// Container — the kagent translator builds the PodTemplate; we just need
	// to confirm key fields made it across the substrate boundary intact.
	require.Len(t, at.Spec.Containers, 1)
	c := at.Spec.Containers[0]
	require.Equal(t, "localhost:5001/substrate-poc-agent:p0", c.Image)

	// Substrate requires Command to be set (Phase 0 finding: it ignores image
	// CMD/ENTRYPOINT and sets Cwd:/). The translator constructs Command from
	// BYO's Cmd + Args. We don't pin the exact form here — just assert it's
	// non-empty so a future translator change to absolute-path commands won't
	// silently produce a broken ActorTemplate.
	require.NotEmpty(t, c.Command, "Container.Command must be set; substrate ignores image CMD/ENTRYPOINT")
	require.Equal(t, "/usr/local/bin/uvicorn", c.Command[0])
}
