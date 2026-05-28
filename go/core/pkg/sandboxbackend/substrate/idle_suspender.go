package substrate

import (
	"context"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// IdleSuspender records the last time a substrate actor was touched (saw
// a request through the kagent A2A mux) and periodically asks substrate to
// suspend any actor that's been idle longer than a configurable timeout.
//
// State lives in-memory only (per controller process). On controller restart,
// timers reset; in-flight RUNNING actors will sit RUNNING until traffic
// re-touches them, at which point the new timer takes over. That's an
// acceptable degradation for a PoC — see SUBSTRATE.md §11 for production
// notes.
//
// The component implements controller-runtime's manager.Runnable so its
// sweep goroutine starts and stops with the manager lifecycle.
type IdleSuspender struct {
	control       *ControlClient
	idleTimeout   time.Duration
	sweepInterval time.Duration

	mu        sync.Mutex
	lastTouch map[string]time.Time
}

var _ manager.Runnable = (*IdleSuspender)(nil)

// IdleSuspenderConfig governs an IdleSuspender's behavior. Both durations
// must be > 0; SweepInterval should be smaller than IdleTimeout (otherwise
// actors stay running well past the timeout simply because we don't notice).
type IdleSuspenderConfig struct {
	Control       *ControlClient
	IdleTimeout   time.Duration
	SweepInterval time.Duration
}

// NewIdleSuspender constructs an IdleSuspender. Returns nil when Control is
// nil — the controller still functions, it just never auto-suspends. This
// matches the spirit of the rest of the substrate backend (degrade gracefully
// when not all wiring is in place).
func NewIdleSuspender(cfg IdleSuspenderConfig) *IdleSuspender {
	if cfg.Control == nil {
		return nil
	}
	if cfg.IdleTimeout <= 0 || cfg.SweepInterval <= 0 {
		return nil
	}
	return &IdleSuspender{
		control:       cfg.Control,
		idleTimeout:   cfg.IdleTimeout,
		sweepInterval: cfg.SweepInterval,
		lastTouch:     make(map[string]time.Time),
	}
}

// Touch records that the given actor saw activity now. Safe to call
// concurrently. No-op when receiver is nil.
func (s *IdleSuspender) Touch(actorID string) {
	if s == nil || actorID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.lastTouch[actorID] = now
	s.mu.Unlock()
}

// NeedLeaderElection: only one replica needs to be running the sweep loop.
// Returning true ensures the sweeper doesn't double-suspend in HA deploys.
func (s *IdleSuspender) NeedLeaderElection() bool { return true }

// Start runs the sweep loop until ctx is canceled.
func (s *IdleSuspender) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("substrate-idle-suspender")
	log.Info("starting", "idleTimeout", s.idleTimeout, "sweepInterval", s.sweepInterval)
	t := time.NewTicker(s.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-t.C:
			s.sweep(ctx, log.WithValues("op", "sweep"))
		}
	}
}

// sweep finds actors that haven't been touched within idleTimeout, calls
// SuspendActor on each, and forgets them. Errors are logged but don't abort
// the sweep — one flaky Suspend shouldn't block the rest.
func (s *IdleSuspender) sweep(ctx context.Context, log interface {
	Info(string, ...any)
	Error(error, string, ...any)
}) {
	expired := s.expiredActors(time.Now())
	if len(expired) == 0 {
		return
	}
	for _, actorID := range expired {
		if err := s.control.SuspendActor(ctx, actorID); err != nil {
			log.Error(err, "SuspendActor failed; will retry next sweep", "actor", actorID)
			// Don't forget the entry — next sweep will retry. If the actor
			// was already SUSPENDED the ControlClient swallows the error, so
			// this path only fires on genuine RPC failures.
			s.touchToReset(actorID)
			continue
		}
		log.Info("auto-suspended idle actor", "actor", actorID)
		s.forget(actorID)
	}
}

// expiredActors snapshots the keys whose last-touch is older than the
// timeout. Returned outside the lock so the SuspendActor RPC isn't held by
// the mutex.
func (s *IdleSuspender) expiredActors(now time.Time) []string {
	cutoff := now.Add(-s.idleTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, last := range s.lastTouch {
		if last.Before(cutoff) {
			out = append(out, id)
		}
	}
	return out
}

func (s *IdleSuspender) forget(actorID string) {
	s.mu.Lock()
	delete(s.lastTouch, actorID)
	s.mu.Unlock()
}

// touchToReset is used when a sweep tries to suspend an actor but the RPC
// fails. Resetting the touch time means we don't hammer ate-api-server with
// retries every sweepInterval; we wait one full idleTimeout before trying
// again.
func (s *IdleSuspender) touchToReset(actorID string) {
	s.Touch(actorID)
}
