package substrate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ControlClient is a thin wrapper around the substrate `ateapi.Control` gRPC
// client. It provides idempotent helpers for the actor lifecycle operations
// the kagent reconciler needs (CreateActor on Ready, SuspendActor on idle).
//
// The PoC matches kubectl-ate's posture: TLS with InsecureSkipVerify. Real
// production hardening (pod-cert mTLS via `podcert.ate.dev`) is out of scope.
type ControlClient struct {
	conn    *grpc.ClientConn
	control ateapipb.ControlClient
}

// ControlClientConfig governs how ControlClient connects to the substrate
// `api.ate-system.svc` Service.
type ControlClientConfig struct {
	// Endpoint is the gRPC target, e.g. `api.ate-system.svc.cluster.local:443`.
	Endpoint string

	// PlaintextOnly disables TLS entirely. Only for tests / very-local dev.
	PlaintextOnly bool
}

// NewControlClient dials the substrate Control API. The returned client is
// safe for concurrent use. Callers must invoke Close when done.
func NewControlClient(ctx context.Context, cfg ControlClientConfig) (*ControlClient, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("substrate Control endpoint is empty")
	}
	var creds credentials.TransportCredentials
	if cfg.PlaintextOnly {
		creds = insecure.NewCredentials()
	} else {
		// Matches kubectl-ate's dial posture: TLS with InsecureSkipVerify.
		// The substrate apiserver presents a pod certificate signed by
		// podcert.ate.dev that isn't part of the controller's trust store
		// (yet). Production must use proper CA verification.
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial substrate control %s: %w", cfg.Endpoint, err)
	}
	return &ControlClient{conn: conn, control: ateapipb.NewControlClient(conn)}, nil
}

// Close releases the underlying gRPC connection.
func (c *ControlClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CreateActorIfMissing creates an actor record in substrate idempotently.
//
// Substrate returns codes.AlreadyExists when an actor with the given id
// exists; that's treated as success here. Any other gRPC error is returned
// verbatim so the caller can decide whether to retry.
//
// On success the actor is in STATUS_SUSPENDED until a request triggers
// atenet's ResumeActor flow.
func (c *ControlClient) CreateActorIfMissing(ctx context.Context, actorID, templateNS, templateName string) (*ateapipb.Actor, error) {
	resp, err := c.control.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId:                actorID,
		ActorTemplateNamespace: templateNS,
		ActorTemplateName:      templateName,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			// Fetch the existing actor so the caller still gets a useful return
			// value (status, current worker, last snapshot, etc.).
			return c.GetActor(ctx, actorID)
		}
		return nil, fmt.Errorf("CreateActor %s (template %s/%s): %w", actorID, templateNS, templateName, err)
	}
	return resp.GetActor(), nil
}

// GetActor fetches an actor's current state. Returns nil + nil if the actor
// doesn't exist, so callers can distinguish "missing" from "lookup failed."
func (c *ControlClient) GetActor(ctx context.Context, actorID string) (*ateapipb.Actor, error) {
	resp, err := c.control.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: actorID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("GetActor %s: %w", actorID, err)
	}
	return resp.GetActor(), nil
}

// SuspendActor evicts a running actor back to its snapshot. Idempotent: a
// SuspendActor against an already-SUSPENDED actor returns FailedPrecondition,
// which we treat as success.
func (c *ControlClient) SuspendActor(ctx context.Context, actorID string) error {
	_, err := c.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: actorID})
	if err != nil {
		// SuspendActor on a not-running actor is a no-op for our purposes;
		// substrate signals it as FailedPrecondition or NotFound depending on
		// state. Don't fail loud.
		if c := status.Code(err); c == codes.FailedPrecondition || c == codes.NotFound {
			return nil
		}
		return fmt.Errorf("SuspendActor %s: %w", actorID, err)
	}
	return nil
}

// DeleteActor removes the actor record from substrate. Idempotent on NotFound.
// Substrate requires actors to be SUSPENDED before delete; if the actor is in
// a non-SUSPENDED state substrate returns FailedPrecondition — callers should
// use DeleteActorSequenced for the safe lifecycle (suspend-then-delete).
func (c *ControlClient) DeleteActor(ctx context.Context, actorID string) error {
	_, err := c.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: actorID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("DeleteActor %s: %w", actorID, err)
	}
	return nil
}
