// Package tui is Orrery's terminal interface. It is a transport adapter over
// the same session contract as the web UI: one TUI process is bound to exactly
// one session, renders that session's durable event log, and submits messages
// through the engine's idempotent, queue-aware continuation path.
package tui

import (
	"context"
	"errors"

	"github.com/ductone/orrey/internal/store"
)

// Event is the durable session event envelope shared by SQLite, SSE, and the
// TUI. Sequence numbers are per session and strictly increasing.
type Event = store.Event

// ErrNotFound is returned when a session lookup has no match.
var ErrNotFound = errors.New("session not found")

// CreateRequest starts a new session whose first turn runs Prompt. When
// Integration and ExternalID are set, creation is idempotent on that external
// identity, exactly like POST /api/v1/sessions.
type CreateRequest struct {
	Prompt              string
	Workspace           string
	BudgetUSD           float64
	TierPin             string
	Integration         string
	ExternalID          string
	ExternalIncarnation string
}

// SendResult reports how the engine accepted a message. Queued means a turn
// was already running and the message will be delivered as the next turn.
type SendResult struct {
	Queued    bool
	Duplicate bool
}

// Backend is the session surface the TUI needs. The local implementation
// drives an in-process engine; the remote implementation speaks the
// /api/v1 HTTP and SSE contract of `orrery serve`. Every method is safe for
// concurrent use.
type Backend interface {
	// Describe names the backend for the footer, e.g. "local" or a server URL.
	Describe() string
	Session(ctx context.Context, id string) (store.Session, error)
	// Lookup finds the session bound to an external identity. It returns
	// ErrNotFound when no session exists.
	Lookup(ctx context.Context, integration, externalID, incarnation string) (store.Session, error)
	Create(ctx context.Context, req CreateRequest) (string, error)
	Send(ctx context.Context, sessionID, content, requestID string) (SendResult, error)
	// Stream delivers every event with a sequence greater than after, in
	// order, as batches on out. It blocks until ctx is cancelled and returns
	// ctx.Err() then; transient transport failures are retried internally.
	Stream(ctx context.Context, sessionID string, after int, out chan<- []Event) error
	Cancel(ctx context.Context, sessionID string) (bool, error)
	Compact(ctx context.Context, sessionID string) error
	Checkpoint(ctx context.Context, sessionID, label string) (store.Checkpoint, error)
	Checkpoints(ctx context.Context, sessionID string) ([]store.Checkpoint, error)
	Restore(ctx context.Context, sessionID, checkpointID string) error
	// AddBudget raises the spend ceiling and reports whether a session that
	// stopped at its ceiling resumed.
	AddBudget(ctx context.Context, sessionID string, addUSD float64) (bool, error)
}
