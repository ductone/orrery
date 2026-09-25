package tui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/store"
)

// localPoll matches the SSE transport's cadence closely enough that local
// and remote sessions feel identical, while staying cheap on SQLite.
const localPoll = 120 * time.Millisecond

// Local drives an engine inside the TUI process. Turns inherit base, not a
// request context, so they outlive the call that started them.
type Local struct {
	engine *core.Engine
	base   context.Context
}

func NewLocal(base context.Context, engine *core.Engine) *Local {
	return &Local{engine: engine, base: base}
}

func (l *Local) Describe() string { return "local" }

func (l *Local) Session(ctx context.Context, id string) (store.Session, error) {
	s, err := l.engine.Store().Session(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s, err
}

func (l *Local) Lookup(ctx context.Context, integration, externalID, incarnation string) (store.Session, error) {
	s, err := l.engine.Store().SessionByExternalID(ctx, integration, externalID, incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

func (l *Local) Create(_ context.Context, req CreateRequest) (string, error) {
	task := agentproto.TaskRequest{
		Spec:      req.Prompt,
		Budget:    agentproto.Budget{MaxUSD: req.BudgetUSD},
		Workspace: agentproto.Workspace{Path: req.Workspace, Mode: "shared-write"},
		Hints:     agentproto.RoutingHints{TierPin: req.TierPin},
	}
	if req.Integration == "" {
		id, _, err := l.engine.Start(l.base, task, nil)
		return id, err
	}
	info, err := l.engine.StartIntegrated(l.base, task, core.SessionOptions{
		Integration:         req.Integration,
		ExternalID:          req.ExternalID,
		ExternalIncarnation: req.ExternalIncarnation,
		RequestID:           uuid.NewString(),
		WorkspaceOwnership:  "external",
	}, nil)
	return info.SessionID, err
}

func (l *Local) Send(_ context.Context, sessionID, content, requestID string) (SendResult, error) {
	info, err := l.engine.ContinueIntegrated(l.base, sessionID, content, requestID, "tui", nil)
	return SendResult{Queued: info.Queued, Duplicate: info.Duplicate}, err
}

func (l *Local) Stream(ctx context.Context, sessionID string, after int, out chan<- []Event) error {
	tick := time.NewTicker(localPoll)
	defer tick.Stop()
	for {
		events, err := l.engine.Store().EventsAfter(ctx, sessionID, after)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && len(events) > 0 {
			select {
			case out <- events:
				after = events[len(events)-1].Seq
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (l *Local) Cancel(_ context.Context, sessionID string) (bool, error) {
	return l.engine.Cancel(sessionID), nil
}

func (l *Local) Compact(ctx context.Context, sessionID string) error {
	return l.engine.Compact(ctx, sessionID, "manual", nil)
}

func (l *Local) Checkpoint(ctx context.Context, sessionID, label string) (store.Checkpoint, error) {
	return l.engine.Checkpoint(ctx, sessionID, label)
}

func (l *Local) Checkpoints(ctx context.Context, sessionID string) ([]store.Checkpoint, error) {
	return l.engine.Store().Checkpoints(ctx, sessionID)
}

func (l *Local) Restore(ctx context.Context, sessionID, checkpointID string) error {
	return l.engine.RestoreCheckpoint(ctx, sessionID, checkpointID, nil)
}

func (l *Local) AddBudget(_ context.Context, sessionID string, addUSD float64) (bool, error) {
	_, resumed, err := l.engine.AddBudget(l.base, sessionID, addUSD, nil)
	return resumed, err
}
