package core

import (
	"context"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/store"
)

// Read-only workers synthesise their result at a turn limit: the spawner's
// (a review plan sizes it to the diff) or defaultWorkerTurns. A Jev check of
// whether a worker had converged used to run past that limit; in practice it
// never once judged a worker done, so it only added a call per turn.
const defaultWorkerTurns = 4

// workerTurnLimit returns the turn at which a read-only worker synthesises.
func workerTurnLimit(req agentproto.TaskRequest) int {
	if req.Hints.WorkerTurns > 0 {
		return req.Hints.WorkerTurns
	}
	return defaultWorkerTurns
}

// synthesisDue reports whether a read-only worker must synthesise on this turn.
func (e *Engine) synthesisDue(ctx context.Context, sid, spec string, req agentproto.TaskRequest, turn int, stored []store.Message, emit EmitFunc) bool {
	return req.Workspace.Mode == "read" && turn >= workerTurnLimit(req)
}
