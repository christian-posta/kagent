package substrate

import (
	"context"
	"testing"

	substratev1 "github.com/agent-substrate/substrate/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newEnsurerForTest(t *testing.T, ateomImage string, replicas int32, seed ...client.Object) (*WorkerPoolEnsurer, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, substratev1.AddToScheme(scheme))
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).Build()

	e := NewWorkerPoolEnsurer("kagent-substrate-poc", "poc-pool", ateomImage, replicas)
	require.NotNil(t, e, "NewWorkerPoolEnsurer returned nil for valid inputs")
	e.clientFactory = func() (client.Client, error) { return cli, nil }
	return e, cli
}

func TestWorkerPoolEnsurer_NewReturnsNilOnEmptyImage(t *testing.T) {
	require.Nil(t, NewWorkerPoolEnsurer("ns", "name", "", 2),
		"empty AteomImage means no-auto-provision; constructor should return nil")
	require.Nil(t, NewWorkerPoolEnsurer("", "name", "img", 2),
		"empty namespace should also return nil")
	require.Nil(t, NewWorkerPoolEnsurer("ns", "", "img", 2),
		"empty name should also return nil")
}

func TestWorkerPoolEnsurer_DefaultsReplicasWhenZero(t *testing.T) {
	e := NewWorkerPoolEnsurer("ns", "name", "img", 0)
	require.NotNil(t, e)
	require.EqualValues(t, 2, e.Replicas, "replicas should default to 2 when caller passes <= 0")
}

func TestWorkerPoolEnsurer_NilReceiver_IsNoOp(t *testing.T) {
	var e *WorkerPoolEnsurer
	require.NoError(t, e.Start(context.Background()),
		"Start on a nil ensurer must not panic and must return nil error so the manager doesn't blow up")
}

func TestWorkerPoolEnsurer_CreatesWhenMissing(t *testing.T) {
	e, cli := newEnsurerForTest(t, "ateom:v1", 3)
	require.NoError(t, e.Start(context.Background()))

	got := &substratev1.WorkerPool{}
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "kagent-substrate-poc", Name: "poc-pool",
	}, got))
	require.Equal(t, "ateom:v1", got.Spec.AteomImage)
	require.EqualValues(t, 3, got.Spec.Replicas)
	require.Equal(t, "kagent", got.Labels["app.kubernetes.io/managed-by"],
		"ensurer must label the WorkerPool as kagent-managed so operators can tell it apart")
}

func TestWorkerPoolEnsurer_NoOpWhenMatching(t *testing.T) {
	existing := &substratev1.WorkerPool{}
	existing.Name = "poc-pool"
	existing.Namespace = "kagent-substrate-poc"
	existing.Spec.AteomImage = "ateom:v1"
	existing.Spec.Replicas = 7 // operator scaled it manually

	e, cli := newEnsurerForTest(t, "ateom:v1", 2, existing)
	require.NoError(t, e.Start(context.Background()))

	got := &substratev1.WorkerPool{}
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "kagent-substrate-poc", Name: "poc-pool",
	}, got))
	require.Equal(t, "ateom:v1", got.Spec.AteomImage)
	require.EqualValues(t, 7, got.Spec.Replicas,
		"ensurer must NOT revert operator-scaled replicas when ateomImage already matches")
}

func TestWorkerPoolEnsurer_PatchesAteomImageWhenDrifted(t *testing.T) {
	existing := &substratev1.WorkerPool{}
	existing.Name = "poc-pool"
	existing.Namespace = "kagent-substrate-poc"
	existing.Spec.AteomImage = "ateom:OLD"
	existing.Spec.Replicas = 7 // operator-scaled

	e, cli := newEnsurerForTest(t, "ateom:NEW", 2, existing)
	require.NoError(t, e.Start(context.Background()))

	got := &substratev1.WorkerPool{}
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "kagent-substrate-poc", Name: "poc-pool",
	}, got))
	require.Equal(t, "ateom:NEW", got.Spec.AteomImage,
		"ensurer must patch ateomImage when the flag-configured value drifts from the cluster")
	require.EqualValues(t, 7, got.Spec.Replicas,
		"even on patch, ensurer must not touch replicas — operators own that field")
}
