package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ductone/orrey/internal/store"
)

// workerWaitTimeout bounds a job wait on a worker. A worker that outlives it
// is still running; the model can wait again or continue, and its handoff
// arrives on completion either way.
const workerWaitTimeout = 10 * time.Minute

var errUnknownJob = errors.New("job not found: no background command or worker job of this session has this id")

// workerJob serves the job tool for worker jobs, which the tool registry
// does not know. Models reasonably reach for job wait after spawn; answering
// "job not found" once led an agent to abandon a worker that later succeeded.
func (e *Engine) workerJob(ctx context.Context, sid, id, action string) (any, error) {
	j, err := e.store.Job(ctx, id)
	if err != nil || j.SessionID != sid {
		return nil, errUnknownJob
	}
	switch action {
	case "cancel":
		return nil, errors.New("worker jobs cannot be cancelled with job; a worker stops at its own budget")
	case "logs":
		return workerJobView(j), nil
	case "wait":
		deadline := time.NewTimer(workerWaitTimeout)
		defer deadline.Stop()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for j.Status == "running" {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				view := workerJobView(j)
				view["hint"] = "The worker is still running. Wait again, or continue; its result arrives as a handoff when it finishes."
				return view, nil
			case <-tick.C:
			}
			if j, err = e.store.Job(ctx, id); err != nil {
				return nil, err
			}
		}
		e.markDelivered(id)
		return workerJobView(j), nil
	default:
		return nil, errors.New("invalid action")
	}
}

func workerJobView(j store.Job) map[string]any {
	view := map[string]any{"id": j.ID, "kind": "worker", "status": j.Status}
	if j.ResultJSON != "" && j.ResultJSON != "null" {
		view["result"] = json.RawMessage(j.ResultJSON)
	}
	return view
}
