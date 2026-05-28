package substrate

import (
	"context"
	"fmt"
	"time"

	ateapipb "github.com/agent-substrate/substrate/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pacing for the suspend-then-delete state machine. Substrate's actor state
// machine transitions through SUSPENDING → SUSPENDED in seconds typically,
// but a stuck `runsc checkpoint` (see SUBSTRATE.md §19) can hold a worker
// for minutes. The 5-minute deadline matches the SandboxAgent finalizer's
// patience: past that, the reconciler removes the finalizer and surfaces a
// warning event so deletion of the K8s CR isn't blocked forever.
const (
	actorDeletePollInterval = 2 * time.Second
	actorDeleteTimeout      = 5 * time.Minute
)

// DeleteActorSequenced drives substrate's required suspend-then-delete state
// machine: ensure the actor is SUSPENDED → DeleteActor → wait for NotFound.
// NotFound at any step is treated as success (idempotent).
//
// Adopted from pj-kagent's `cmd/servers/sandboxbackend/substrate/delete_actor.go`
// because we needed the same recipe — once we hit a wedged actor with a stale
// `last_snapshot` URI (real Phase-A2 finding), the only way to recover was the
// suspend-wait-delete-wait sequence. Mirror, don't fork: this file follows
// the same shape so the two implementations can converge upstream later.
//
// Bounded by actorDeleteTimeout. Callers should wrap with a context whose
// deadline is at least that long; otherwise we surface ctx.Err().
func (c *ControlClient) DeleteActorSequenced(ctx context.Context, actorID string) error {
	if c == nil {
		return fmt.Errorf("DeleteActorSequenced: nil ControlClient")
	}
	if actorID == "" {
		return nil
	}
	deadline := time.Now().Add(actorDeleteTimeout)

	actor, err := c.GetActor(ctx, actorID)
	if err != nil {
		return fmt.Errorf("DeleteActorSequenced %q: pre-check: %w", actorID, err)
	}
	if actor == nil {
		// Already gone.
		return nil
	}

	if err := c.ensureActorSuspended(ctx, actorID, actor.GetStatus(), deadline); err != nil {
		return fmt.Errorf("DeleteActorSequenced %q: ensure suspended: %w", actorID, err)
	}

	if err := c.DeleteActor(ctx, actorID); err != nil {
		// DeleteActor surfaces FailedPrecondition when the actor isn't
		// SUSPENDED — possible if a Resume slipped in between our wait and
		// the delete call. Re-fetch so the error message names the actual
		// status the operator is seeing.
		if status.Code(err) == codes.FailedPrecondition {
			if reread, getErr := c.GetActor(ctx, actorID); getErr == nil && reread != nil {
				return fmt.Errorf("DeleteActorSequenced %q: not suspended (status %s)", actorID, reread.GetStatus())
			}
		}
		return fmt.Errorf("DeleteActorSequenced %q: %w", actorID, err)
	}

	return c.waitForActorDeleted(ctx, actorID, deadline)
}

// ensureActorSuspended takes the actor through whatever transitions are needed
// to reach STATUS_SUSPENDED, blocking until it gets there (or until deadline).
func (c *ControlClient) ensureActorSuspended(ctx context.Context, actorID string, current ateapipb.Actor_Status, deadline time.Time) error {
	switch current {
	case ateapipb.Actor_STATUS_SUSPENDED, ateapipb.Actor_STATUS_UNSPECIFIED:
		return nil
	case ateapipb.Actor_STATUS_SUSPENDING:
		// Already in the right direction. Kick once in case substrate dropped
		// the prior request, then wait.
		_ = c.SuspendActor(ctx, actorID)
		return c.waitForActorStatus(ctx, actorID, ateapipb.Actor_STATUS_SUSPENDED, deadline)
	case ateapipb.Actor_STATUS_RUNNING, ateapipb.Actor_STATUS_RESUMING:
		if err := c.SuspendActor(ctx, actorID); err != nil {
			return fmt.Errorf("SuspendActor: %w", err)
		}
		return c.waitForActorStatus(ctx, actorID, ateapipb.Actor_STATUS_SUSPENDED, deadline)
	default:
		// Unknown / future status — try to suspend anyway and see.
		_ = c.SuspendActor(ctx, actorID)
		return c.waitForActorStatus(ctx, actorID, ateapipb.Actor_STATUS_SUSPENDED, deadline)
	}
}

// waitForActorStatus polls GetActor until the actor reaches the desired status,
// the context is cancelled, or the deadline elapses.
func (c *ControlClient) waitForActorStatus(ctx context.Context, actorID string, want ateapipb.Actor_Status, deadline time.Time) error {
	for time.Now().Before(deadline) {
		actor, err := c.GetActor(ctx, actorID)
		if err != nil {
			return fmt.Errorf("get during wait-for-status: %w", err)
		}
		if actor == nil {
			// NotFound: only treat as success if we were waiting for absence.
			if want == ateapipb.Actor_STATUS_UNSPECIFIED {
				return nil
			}
			return fmt.Errorf("actor disappeared while waiting for %s", want)
		}
		if actor.GetStatus() == want {
			return nil
		}
		if err := sleepOrDone(ctx, actorDeletePollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("timeout waiting for actor status %s", want)
}

// waitForActorDeleted polls GetActor until it returns NotFound (or deadline).
func (c *ControlClient) waitForActorDeleted(ctx context.Context, actorID string, deadline time.Time) error {
	for time.Now().Before(deadline) {
		actor, err := c.GetActor(ctx, actorID)
		if err != nil {
			return fmt.Errorf("get during wait-for-deleted: %w", err)
		}
		if actor == nil {
			return nil
		}
		if err := sleepOrDone(ctx, actorDeletePollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("timeout waiting for actor deletion")
}

// sleepOrDone is select-on-timer-or-context. Returns ctx.Err() if cancelled.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
