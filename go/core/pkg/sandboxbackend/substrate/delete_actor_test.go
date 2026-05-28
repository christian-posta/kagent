package substrate

import (
	"context"
	"sync"
	"testing"
	"time"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
)

func nnOf(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

// fakeActorStore is a stateful fake of substrate's ate-api-server. It tracks
// per-actor status and lets tests script transitions: the ControlClient's
// SuspendActor/DeleteActor calls drive state changes; GetActor reads the
// current state. Concurrent-safe (the close-loop in IdleSuspender already
// hits SuspendActor concurrently with GetActor).
type fakeActorStore struct {
	mu     sync.Mutex
	actors map[string]*ateapipb.Actor

	// Optional knobs to script harder paths.
	delayBeforeSuspended time.Duration // wait this long after SuspendActor before flipping to SUSPENDED
	suspendErrCode       codes.Code    // if != codes.OK, SuspendActor returns this error

	// Telemetry for assertions.
	deletes []string
}

func newFakeActorStore() *fakeActorStore {
	return &fakeActorStore{actors: map[string]*ateapipb.Actor{}}
}

func (f *fakeActorStore) set(id string, st ateapipb.Actor_Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors[id] = &ateapipb.Actor{ActorId: id, Status: st}
}

// ---- ateapipb.ControlClient adapter (only the methods DeleteActorSequenced needs).

func (f *fakeActorStore) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.GetActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.actors[in.GetActorId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such actor")
	}
	return &ateapipb.GetActorResponse{Actor: a}, nil
}

func (f *fakeActorStore) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, _ ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	if f.suspendErrCode != codes.OK {
		return nil, status.Error(f.suspendErrCode, "scripted suspend error")
	}
	id := in.GetActorId()
	f.mu.Lock()
	a, ok := f.actors[id]
	if !ok {
		f.mu.Unlock()
		return nil, status.Error(codes.NotFound, "no such actor")
	}
	switch a.Status {
	case ateapipb.Actor_STATUS_SUSPENDED:
		// Idempotent: nothing to do.
	default:
		a.Status = ateapipb.Actor_STATUS_SUSPENDING
	}
	delay := f.delayBeforeSuspended
	f.mu.Unlock()

	// Asynchronously transition to SUSPENDED so the wait loops have something to observe.
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if cur, ok := f.actors[id]; ok && cur.Status == ateapipb.Actor_STATUS_SUSPENDING {
			cur.Status = ateapipb.Actor_STATUS_SUSPENDED
		}
	}()
	return &ateapipb.SuspendActorResponse{}, nil
}

func (f *fakeActorStore) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, _ ...grpc.CallOption) (*ateapipb.DeleteActorResponse, error) {
	id := in.GetActorId()
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.actors[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such actor")
	}
	if a.Status != ateapipb.Actor_STATUS_SUSPENDED {
		return nil, status.Error(codes.FailedPrecondition, "actor not suspended")
	}
	delete(f.actors, id)
	f.deletes = append(f.deletes, id)
	return &ateapipb.DeleteActorResponse{}, nil
}

// Unused-but-required for ateapipb.ControlClient — return Unimplemented so a
// test that accidentally exercises them fails loudly instead of silently.
func (f *fakeActorStore) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, _ ...grpc.CallOption) (*ateapipb.CreateActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateActor not modeled in fakeActorStore")
}
func (f *fakeActorStore) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, _ ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ResumeActor not modeled in fakeActorStore")
}
func (f *fakeActorStore) ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, _ ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListWorkers not modeled in fakeActorStore")
}
func (f *fakeActorStore) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListActors not modeled in fakeActorStore")
}
func (f *fakeActorStore) DebugClear(ctx context.Context, in *ateapipb.DebugClearRequest, _ ...grpc.CallOption) (*ateapipb.DebugClearResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DebugClear not modeled in fakeActorStore")
}

func buildClient(store *fakeActorStore) *ControlClient {
	return &ControlClient{control: store}
}

// ---- Tests ----

func TestDeleteActorSequenced_AlreadySuspended_DirectDelete(t *testing.T) {
	st := newFakeActorStore()
	st.set("a1", ateapipb.Actor_STATUS_SUSPENDED)

	require.NoError(t, buildClient(st).DeleteActorSequenced(context.Background(), "a1"))
	require.Equal(t, []string{"a1"}, st.deletes, "should call DeleteActor exactly once")
}

func TestDeleteActorSequenced_Running_SuspendsThenDeletes(t *testing.T) {
	st := newFakeActorStore()
	st.set("a1", ateapipb.Actor_STATUS_RUNNING)
	// Short delay so we actually exercise the wait loop (without making the test slow).
	st.delayBeforeSuspended = 250 * time.Millisecond

	start := time.Now()
	require.NoError(t, buildClient(st).DeleteActorSequenced(context.Background(), "a1"))
	require.GreaterOrEqual(t, time.Since(start), st.delayBeforeSuspended,
		"DeleteActorSequenced should have waited for SUSPENDED before deleting")
	require.Equal(t, []string{"a1"}, st.deletes)
}

func TestDeleteActorSequenced_Suspending_WaitsForSuspendedThenDeletes(t *testing.T) {
	st := newFakeActorStore()
	st.set("a1", ateapipb.Actor_STATUS_SUSPENDING)
	st.delayBeforeSuspended = 250 * time.Millisecond

	require.NoError(t, buildClient(st).DeleteActorSequenced(context.Background(), "a1"))
	require.Equal(t, []string{"a1"}, st.deletes)
}

func TestDeleteActorSequenced_NotFound_IsSuccess(t *testing.T) {
	st := newFakeActorStore()
	// Don't seed any actor — GetActor returns NotFound.
	require.NoError(t, buildClient(st).DeleteActorSequenced(context.Background(), "missing"))
	require.Empty(t, st.deletes, "DeleteActor should not be called when GetActor returned NotFound")
}

func TestDeleteActorSequenced_EmptyID_IsNoOp(t *testing.T) {
	st := newFakeActorStore()
	require.NoError(t, buildClient(st).DeleteActorSequenced(context.Background(), ""))
	require.Empty(t, st.deletes)
}

func TestDeleteActorSequenced_NilReceiver_ReturnsError(t *testing.T) {
	var nilClient *ControlClient
	require.Error(t, nilClient.DeleteActorSequenced(context.Background(), "a1"))
}

// OnDelete is the DeletingBackend hook the reconciler calls from the
// finalizer; it should derive the actor ID consistently with ActorIDFor and
// drive DeleteActorSequenced. Cheaper to test here (real ControlClient over
// the fake store) than to stand up envtest.
func TestBackend_OnDelete_DeletesActor(t *testing.T) {
	st := newFakeActorStore()
	st.set(ActorIDFor("ns1", "agent1"), ateapipb.Actor_STATUS_SUSPENDED)

	b := New(Config{Control: buildClient(st)})
	require.NoError(t, b.OnDelete(context.Background(), nnOf("ns1", "agent1")))
	require.Equal(t, []string{ActorIDFor("ns1", "agent1")}, st.deletes,
		"OnDelete must derive actor ID via ActorIDFor and call DeleteActor exactly once")
}

func TestBackend_OnDelete_NoControlClient_IsNoOp(t *testing.T) {
	b := New(Config{Control: nil})
	require.NoError(t, b.OnDelete(context.Background(), nnOf("ns1", "agent1")))
}

func TestBackend_OnDelete_NilReceiver_IsNoOp(t *testing.T) {
	var b *Backend
	require.NoError(t, b.OnDelete(context.Background(), nnOf("ns1", "agent1")))
}

func TestDeleteActorSequenced_ContextCancel_ShortCircuits(t *testing.T) {
	st := newFakeActorStore()
	st.set("a1", ateapipb.Actor_STATUS_RUNNING)
	// Never transition; the wait loop must respond to ctx.Done().
	st.delayBeforeSuspended = 10 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := buildClient(st).DeleteActorSequenced(ctx, "a1")
	require.ErrorIs(t, err, context.Canceled,
		"DeleteActorSequenced should propagate ctx.Canceled, got: %v", err)
}
