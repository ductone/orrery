package core

import (
	"context"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/store"
)

// Read-only workers must eventually stop gathering and synthesise. The turn
// at which they may be asked to (the soft limit) comes from the spawner, such
// as a review plan sized to its diff; past it, Jev judges whether the worker
// has converged, and a hard limit bounds the whole window. Without Jev, the
// soft limit is where synthesis is forced, as it always was.
const (
	defaultWorkerTurns = 4
	// convergedEvidence and staleGround are the probabilities at which a
	// worker counts as done: it has enough evidence, or it has stopped
	// examining anything new.
	convergedEvidence    = 0.7
	staleGround          = 0.3
	convergenceTimeout   = 5 * time.Second
	convergenceSpecChars = 6_000
)

var convergenceQuestions = map[string]jev.Question{
	"enough_evidence": jev.Noul(
		"Has this read-only worker gathered enough evidence to produce its required result?",
		"Its tool calls have covered what the task asks about, and further reading would not change its result.",
		"Parts of the task are still unexamined, or the evidence so far is not enough to answer it.",
	),
	"new_ground": jev.Noul(
		"Are the worker's most recent calls examining parts of the task it had not examined before?",
		"The latest calls read or search things the earlier calls did not cover.",
		"The latest calls repeat, re-read, or circle what earlier calls already covered.",
	),
}

// workerTurnLimits returns a read worker's soft and hard synthesis turns.
func workerTurnLimits(req agentproto.TaskRequest) (soft, hard int) {
	soft = req.Hints.WorkerTurns
	if soft <= 0 {
		soft = defaultWorkerTurns
	}
	return soft, soft + max(2, soft/2)
}

// synthesisDue reports whether a read-only worker must synthesise on this turn.
func (e *Engine) synthesisDue(ctx context.Context, sid, spec string, req agentproto.TaskRequest, turn int, stored []store.Message, emit EmitFunc) bool {
	if req.Workspace.Mode != "read" {
		return false
	}
	soft, hard := workerTurnLimits(req)
	if turn < soft {
		return false
	}
	if turn >= hard {
		e.emit(ctx, sid, "worker.convergence", map[string]any{"turn": turn, "soft_limit": soft, "hard_limit": hard, "synthesize": true, "reason": "hard turn limit"}, emit)
		return true
	}
	cfg, _, _, _, _ := e.runtimeSnapshot()
	if !cfg.Jev.Review || cfg.Jev.APIKey == "" {
		return true
	}
	client := jev.New(cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Model, convergenceTimeout)
	askCtx, cancel := context.WithTimeout(ctx, convergenceTimeout)
	defer cancel()
	state := map[string]any{"task": truncate(spec, convergenceSpecChars), "turn": turn, "recent_tool_calls": activityDigest(stored)}
	resp, err := client.Ask(askCtx, state, convergenceQuestions)
	if err != nil {
		e.emit(ctx, sid, "worker.convergence", map[string]any{"turn": turn, "soft_limit": soft, "hard_limit": hard, "synthesize": true, "reason": "classifier unavailable", "error": err.Error()}, emit)
		return true
	}
	enough, fresh := resp.Answers["enough_evidence"].Noul, resp.Answers["new_ground"].Noul
	if enough == nil || fresh == nil {
		return true
	}
	due := *enough >= convergedEvidence || *fresh < staleGround
	e.emit(ctx, sid, "worker.convergence", map[string]any{"turn": turn, "soft_limit": soft, "hard_limit": hard, "enough_evidence": *enough, "new_ground": *fresh, "synthesize": due}, emit)
	return due
}
