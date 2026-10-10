package core

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// ReviewOutcome is the review cascade's verdict on one prepared change.
type ReviewOutcome struct {
	SessionID string   `json:"session_id"`
	Passed    bool     `json:"passed"`
	Stage     string   `json:"stage"`
	GateScore *float64 `json:"gate_score,omitempty"`
	Findings  []string `json:"findings,omitempty"`
	Seconds   float64  `json:"seconds"`
	CostUSD   float64  `json:"cost_usd"`
}

// reviewCommandTimeout bounds each evidence command a prepared review runs.
const reviewCommandTimeout = 2 * time.Minute

// ReviewChange runs the review cascade on a change, as if an agent had made it
// for task: change applies it to workspace (a git checkout at its base), and
// each command then runs in the workspace, its successful output becoming the
// evidence an agent's own checks would have been. It is for benchmarking the
// review, not for agent runs.
func (e *Engine) ReviewChange(ctx context.Context, workspace, task string, change func() error, commands []string) (ReviewOutcome, error) {
	sid := uuid.NewString()
	if err := e.store.CreateSession(ctx, store.Session{ID: sid, Spec: task, Phase: "review", BudgetUSD: 20, WorkspacePath: workspace}); err != nil {
		return ReviewOutcome{}, err
	}
	e.setBaseline(sid, snapshotWorkspace(ctx, workspace))
	defer e.clearBaseline(sid)
	if err := change(); err != nil {
		return ReviewOutcome{SessionID: sid}, err
	}
	var checks []commandRecord
	for _, command := range commands {
		cmdCtx, cancel := context.WithTimeout(ctx, reviewCommandTimeout)
		cmd := exec.CommandContext(cmdCtx, "sh", "-lc", command)
		cmd.Dir = workspace
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			checks = append(checks, commandRecord{Command: command, Output: outputTail(map[string]any{"summary": string(out)})})
		}
	}
	req := agentproto.TaskRequest{
		Spec:      task,
		Budget:    agentproto.Budget{MaxUSD: 20, MaxTokens: 5_000_000, MaxWallClock: 15 * time.Minute, MaxDepth: 2},
		Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"},
		Depth:     1,
	}
	start := time.Now()
	passed, text, err := e.reviewWorkspace(ctx, sid, "", req, checks, nil)
	out := ReviewOutcome{SessionID: sid, Passed: passed && err == nil, Seconds: time.Since(start).Seconds()}
	var result struct {
		Findings []string `json:"findings"`
	}
	if json.Unmarshal([]byte(text), &result) == nil {
		out.Findings = result.Findings
	}
	out.Stage = "full"
	if events, evErr := e.store.EventsAfter(ctx, sid, 0); evErr == nil {
		for _, ev := range events {
			var data map[string]any
			_ = json.Unmarshal(ev.Data, &data)
			switch ev.Type {
			case "review.gate":
				if score, ok := data["score"].(float64); ok {
					out.GateScore = &score
				}
			case "review.outcome":
				if stage, _ := data["stage"].(string); stage != "" {
					out.Stage = stage
				}
			case "review.plan":
				if skip, _ := data["skip"].(bool); skip {
					out.Stage = "skipped"
				}
			}
		}
	}
	_ = e.store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(spent_usd),0) FROM sessions WHERE id=? OR parent_session_id=?`, sid, sid).Scan(&out.CostUSD)
	return out, err
}
