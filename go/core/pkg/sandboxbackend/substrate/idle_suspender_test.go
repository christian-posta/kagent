package substrate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeControl implements just enough of ateapipb.ControlClient to let us
// observe SuspendActor calls. Tests build a ControlClient around it via the
// internal newControlClientForTesting hook below.
type fakeControl struct {
	ateapipb.UnimplementedControlServer

	mu       sync.Mutex
	suspends []string
	// errOnSuspend forces SuspendActor to fail with the given gRPC code.
	// codes.FailedPrecondition / NotFound are swallowed by ControlClient,
	// so use codes.Unavailable to simulate genuine RPC failure.
	errOnSuspend codes.Code
	calls        atomic.Int64
}

func (f *fakeControl) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	f.calls.Add(1)
	if f.errOnSuspend != codes.OK {
		return nil, status.Error(f.errOnSuspend, "fake error")
	}
	f.mu.Lock()
	f.suspends = append(f.suspends, req.GetActorId())
	f.mu.Unlock()
	return &ateapipb.SuspendActorResponse{}, nil
}

func (f *fakeControl) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.suspends))
	copy(out, f.suspends)
	return out
}

// newControlClientForTesting wraps a raw ateapipb.ControlClient (in our case
// a directly-constructed gRPC stub backed by the fakeControl server) inside
// the same ControlClient envelope production code uses. Hidden behind a
// build-tag-free file because it's only useful to tests in this package.
func newControlClientForTesting(stub ateapipb.ControlClient) *ControlClient {
	return &ControlClient{control: stub}
}

// directStubControlClient skips the gRPC machinery entirely by adapting the
// fakeControl server directly into a ControlClient. Faster than spinning up
// a bufconn server and avoids dependency on grpc.Server.
type directStub struct{ srv *fakeControl }

func (d *directStub) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.GetActorResponse, error) {
	return d.srv.GetActor(ctx, in)
}
func (d *directStub) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.CreateActorResponse, error) {
	return d.srv.CreateActor(ctx, in)
}
func (d *directStub) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	return d.srv.SuspendActor(ctx, in)
}
func (d *directStub) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	return d.srv.ResumeActor(ctx, in)
}
func (d *directStub) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.DeleteActorResponse, error) {
	return d.srv.DeleteActor(ctx, in)
}
func (d *directStub) ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	return d.srv.ListWorkers(ctx, in)
}
func (d *directStub) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	return d.srv.ListActors(ctx, in)
}
func (d *directStub) DebugClear(ctx context.Context, in *ateapipb.DebugClearRequest, opts ...grpc.CallOption) (*ateapipb.DebugClearResponse, error) {
	return d.srv.DebugClear(ctx, in)
}

func buildIdleSuspenderForTest(t *testing.T, srv *fakeControl, timeout, sweep time.Duration) *IdleSuspender {
	t.Helper()
	cli := newControlClientForTesting(&directStub{srv: srv})
	is := NewIdleSuspender(IdleSuspenderConfig{
		Control:       cli,
		IdleTimeout:   timeout,
		SweepInterval: sweep,
	})
	require.NotNil(t, is, "NewIdleSuspender returned nil with valid inputs")
	return is
}

func TestIdleSuspender_NewReturnsNilOnInvalidInputs(t *testing.T) {
	require.Nil(t, NewIdleSuspender(IdleSuspenderConfig{}), "nil Control must produce nil")
	cli := newControlClientForTesting(&directStub{srv: &fakeControl{}})
	require.Nil(t, NewIdleSuspender(IdleSuspenderConfig{Control: cli, IdleTimeout: 0, SweepInterval: time.Second}))
	require.Nil(t, NewIdleSuspender(IdleSuspenderConfig{Control: cli, IdleTimeout: time.Second, SweepInterval: 0}))
}

func TestIdleSuspender_TouchAndExpired(t *testing.T) {
	timeout := 60 * time.Millisecond
	srv := &fakeControl{}
	is := buildIdleSuspenderForTest(t, srv, timeout, 20*time.Millisecond)

	// Touch both at ≈ t0.
	is.Touch("a")
	is.Touch("b")

	// At t0, nothing is expired (clock matches the touches).
	require.Empty(t, is.expiredActors(time.Now()))

	// Wait past the timeout — both touches are now older than `timeout`.
	time.Sleep(timeout + 20*time.Millisecond)
	require.Len(t, is.expiredActors(time.Now()), 2)

	// Re-touch "a" so its last-time is "now"; "b" remains old.
	is.Touch("a")
	exp := is.expiredActors(time.Now())
	require.ElementsMatch(t, []string{"b"}, exp, "only 'b' should be expired after re-touching 'a'")
}

func TestIdleSuspender_SweepCallsSuspendAndForgets(t *testing.T) {
	srv := &fakeControl{}
	is := buildIdleSuspenderForTest(t, srv, 50*time.Millisecond, 20*time.Millisecond)

	is.Touch("alpha")
	is.Touch("beta")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = is.Start(ctx) }()

	require.Eventually(t, func() bool {
		return len(srv.snapshot()) == 2
	}, 2*time.Second, 25*time.Millisecond, "both actors should be auto-suspended after idle")

	// After successful suspend the entries must be forgotten so a fresh touch
	// starts a fresh timer rather than tripping a stale "still idle" check.
	is.mu.Lock()
	require.Empty(t, is.lastTouch)
	is.mu.Unlock()

	// And a NEW touch should not re-trigger Suspend by itself.
	is.Touch("alpha")
	time.Sleep(25 * time.Millisecond) // less than idle timeout
	require.ElementsMatch(t, []string{"alpha", "beta"}, srv.snapshot(),
		"a touch within the idle window must NOT cause a new Suspend; expected exactly the original two suspends")
}

func TestIdleSuspender_StopsOnContextCancel(t *testing.T) {
	srv := &fakeControl{}
	is := buildIdleSuspenderForTest(t, srv, time.Hour, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- is.Start(ctx) }()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not return promptly after ctx cancel")
	}
}

func TestIdleSuspender_SuspendFailureKeepsEntryButResetsTimer(t *testing.T) {
	srv := &fakeControl{errOnSuspend: codes.Unavailable}
	is := buildIdleSuspenderForTest(t, srv, 30*time.Millisecond, 15*time.Millisecond)

	is.Touch("flaky")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = is.Start(ctx) }()

	// Wait for at least one sweep attempt.
	require.Eventually(t, func() bool {
		return srv.calls.Load() >= 1
	}, time.Second, 10*time.Millisecond)

	// Entry must NOT be forgotten (so future sweeps will retry), but the
	// timer was reset — so we shouldn't see a torrent of retries every
	// sweepInterval. Sample again after a couple of sweep intervals: the
	// number of calls should still be <= a small handful.
	time.Sleep(40 * time.Millisecond)
	got := srv.calls.Load()
	require.LessOrEqual(t, got, int64(3),
		"on RPC failure, sweep should back off (timer reset) — saw %d calls", got)

	is.mu.Lock()
	_, present := is.lastTouch["flaky"]
	is.mu.Unlock()
	require.True(t, present, "failed-Suspend entries must remain in the map for later retry")
}

func TestIdleSuspender_TouchOnNilReceiver(t *testing.T) {
	// Defensive: nil-Touch must be a no-op so callers can wire it without
	// nil checks.
	var s *IdleSuspender
	require.NotPanics(t, func() { s.Touch("x") })
}
