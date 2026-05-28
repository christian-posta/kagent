package substrate

import (
	"context"
	"errors"
	"fmt"

	substratev1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// WorkerPoolEnsurer makes the shared substrate WorkerPool a managed-by-kagent
// resource. The operator stops needing to apply 01-substrate.yaml by hand —
// the controller creates the WorkerPool from `--substrate-worker-pool-*`
// flags at startup, and reconciles its ateomImage when the flag changes.
//
// Adopted (in spirit) from pj-kagent's Provisioner pattern. Differences:
//   - We provision once at startup, not per-AgentHarness. Substrate's
//     worker model is one shared pool, so per-agent provisioning is wrong
//     shape.
//   - We don't manage replicas after create — operators routinely scale
//     these pools manually, and an ensurer that reverts that is hostile.
//   - We don't delete on shutdown — restarting kagent shouldn't tear down
//     the worker pool.
//
// Implements controller-runtime's manager.Runnable so its Start runs after
// the manager's cache is up. Leader-elected (only one replica auto-
// provisions in HA deploys).
type WorkerPoolEnsurer struct {
	Namespace  string
	Name       string
	AteomImage string
	Replicas   int32

	// clientFactory builds the kube client lazily so callers don't need a
	// `client.Client` at construction time (handy because we're constructed
	// inside ExtensionConfig before the manager exists). Override in tests.
	clientFactory func() (client.Client, error)
}

var _ manager.Runnable = (*WorkerPoolEnsurer)(nil)

// NewWorkerPoolEnsurer validates inputs and returns the ensurer or nil if
// the configuration says "don't auto-provision" (empty AteomImage). nil is
// a valid no-op runnable from the manager's perspective — the wiring code
// must skip mgr.Add when this returns nil.
func NewWorkerPoolEnsurer(namespace, name, ateomImage string, replicas int32) *WorkerPoolEnsurer {
	if ateomImage == "" || namespace == "" || name == "" {
		return nil
	}
	if replicas <= 0 {
		replicas = 2
	}
	return &WorkerPoolEnsurer{
		Namespace:  namespace,
		Name:       name,
		AteomImage: ateomImage,
		Replicas:   replicas,
	}
}

// NeedLeaderElection returns true so only one controller replica runs the
// ensurer at a time in HA deploys.
func (e *WorkerPoolEnsurer) NeedLeaderElection() bool { return true }

// Start performs the get-or-create-or-patch dance once, then returns.
// manager.Runnable lifecycle: returning nil from Start tells the manager
// the work completed cleanly; the manager doesn't restart us.
func (e *WorkerPoolEnsurer) Start(ctx context.Context) error {
	if e == nil {
		return nil
	}
	log := logf.FromContext(ctx).WithName("substrate-workerpool-ensurer")

	cli, err := e.buildClient()
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	key := types.NamespacedName{Namespace: e.Namespace, Name: e.Name}
	existing := &substratev1.WorkerPool{}
	getErr := cli.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		wp := &substratev1.WorkerPool{
			TypeMeta: metav1.TypeMeta{
				APIVersion: substratev1.GroupVersion.String(),
				Kind:       "WorkerPool",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      e.Name,
				Namespace: e.Namespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kagent"},
			},
			Spec: substratev1.WorkerPoolSpec{
				Replicas:   e.Replicas,
				AteomImage: e.AteomImage,
			},
		}
		if err := cli.Create(ctx, wp); err != nil {
			return fmt.Errorf("create WorkerPool %s: %w", key, err)
		}
		log.Info("created WorkerPool", "workerpool", key.String(), "replicas", e.Replicas, "ateomImage", e.AteomImage)
		return nil

	case getErr != nil:
		return fmt.Errorf("get WorkerPool %s: %w", key, getErr)

	default:
		// WorkerPool exists. Patch the ateomImage if it drifted from the
		// configured value, but DON'T touch replicas — operators routinely
		// scale these manually and reverting that is bad UX.
		if existing.Spec.AteomImage == e.AteomImage {
			log.Info("WorkerPool already matches configured ateomImage; no action", "workerpool", key.String())
			return nil
		}
		before := existing.DeepCopy()
		existing.Spec.AteomImage = e.AteomImage
		if err := cli.Patch(ctx, existing, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch WorkerPool %s: %w", key, err)
		}
		log.Info("patched WorkerPool ateomImage",
			"workerpool", key.String(),
			"from", before.Spec.AteomImage,
			"to", e.AteomImage,
		)
		return nil
	}
}

// buildClient creates a standalone controller-runtime client. We don't go
// through the manager's cache because (a) the manager isn't readily
// reachable from here and (b) the ensurer's read pattern (one Get, one
// Create-or-Patch) doesn't benefit from a cache anyway.
func (e *WorkerPoolEnsurer) buildClient() (client.Client, error) {
	if e.clientFactory != nil {
		return e.clientFactory()
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := substratev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// errMissingAteomImage is exported only for test introspection — callers
// don't need to handle it (NewWorkerPoolEnsurer returns nil instead).
var errMissingAteomImage = errors.New("workerpool ensurer requires a non-empty ateomImage")
