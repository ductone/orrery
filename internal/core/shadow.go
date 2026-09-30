package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/shadow"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// Shadow state limits. Jev is billed per input token and has no documented
// state ceiling, but a shadow observer should not ship an entire session.
const (
	shadowSpecChars    = 8_000
	shadowSummaryChars = 4_000
	shadowDiffChars    = 60_000
	shadowReplyChars   = 4_000
	shadowReplyTurns   = 3
)

// shadowAsk records a shadow question and asks Jev in the background. It
// returns the observation id for attaching a baseline or outcome later, or ""
// when nothing was recorded.
//
// Shadowing is inert by construction: the caller never waits for or reads the
// answer, the call runs on its own timeout detached from the turn, and a
// failure is recorded rather than surfaced. Answers are not added to session
// spend, so enabling shadowing cannot change budget behaviour either.
func (e *Engine) shadowAsk(ctx context.Context, sid, site, version string, turn int, state any, questions map[string]jev.Question, baseline any) string {
	if len(questions) == 0 {
		return ""
	}
	cfg, _, _, _, _ := e.runtimeSnapshot()
	id := uuid.NewString()
	obs := store.ShadowObservation{ID: id, SessionID: sid, TurnID: e.currentTurnID(ctx, sid), Turn: turn, Site: site, QuestionVersion: version, Questions: questions, State: state, Baseline: baseline}
	if err := e.store.CreateShadow(context.WithoutCancel(ctx), obs); err != nil {
		return ""
	}
	client := jev.New(cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Model, cfg.Jev.Timeout())
	e.shadowWG.Add(1)
	go func() {
		defer e.shadowWG.Done()
		callCtx, cancel := context.WithTimeout(context.Background(), cfg.Jev.Timeout())
		defer cancel()
		start := time.Now()
		resp, err := client.Ask(callCtx, state, questions)
		_ = e.store.CompleteShadow(context.Background(), id, resp.Model, resp.Answers, resp.Usage, time.Since(start), err)
	}()
	return id
}

// shadowUpdate attaches a baseline or outcome to an observation, ignoring
// observations that were never recorded.
func (e *Engine) shadowUpdate(id string, set func(context.Context, string, any) error, v any) {
	if id != "" {
		_ = set(context.Background(), id, v)
	}
}

// waitShadows gives in-flight shadow calls one timeout to land before the
// store closes, so a headless run does not lose its last observations.
func (e *Engine) waitShadows() {
	done := make(chan struct{})
	go func() { e.shadowWG.Wait(); close(done) }()
	cfg, _, _, _, _ := e.runtimeSnapshot()
	select {
	case <-done:
	case <-time.After(cfg.Jev.Timeout() + time.Second):
	}
}

func (e *Engine) currentTurnID(ctx context.Context, sid string) string {
	if id := turnIDFromContext(ctx); id != "" {
		return id
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turnIDs[sid]
}

// shadowTurn cross-checks the phase the router used and scores the remaining
// work, after the real routing decision has been made.
func (e *Engine) shadowTurn(ctx context.Context, s store.Session, stored []store.Message, state router.RoutingState, decision router.Decision) {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	questions := shadow.TurnQuestions(cfg.Jev.Shadows("phase"), cfg.Jev.Shadows("difficulty"))
	if len(questions) == 0 {
		return
	}
	todos, _ := e.store.Todos(ctx, s.ID)
	plan := make([]map[string]string, 0, len(todos))
	for _, t := range todos {
		plan = append(plan, map[string]string{"text": t.Text, "phase": t.Phase, "status": t.Status})
	}
	view := map[string]any{
		"task":              truncate(s.Spec, shadowSpecChars),
		"plan":              plan,
		"recent_tool_calls": activityDigest(stored),
	}
	if s.DurableSummary != "" {
		view["durable_summary"] = truncate(s.DurableSummary, shadowSummaryChars)
	}
	if state.NewInstruction {
		view["latest_user_message"] = truncate(lastUserText(stored), shadowSpecChars)
	}
	baseline := map[string]any{
		"declared_phase": s.Phase, "routed_phase": string(state.Phase), "point": string(state.Point), "worker": s.ParentSessionID != "",
		"phase_source":    phaseSource(state),
		"new_instruction": state.NewInstruction, "model": decision.Model.ID, "tier": string(decision.Model.Tier), "effort": string(decision.Effort),
	}
	e.shadowAsk(ctx, s.ID, shadow.Turn, shadow.TurnVersion, state.Turn, view, questions, baseline)
}

// lastUserText returns the latest message a person sent, skipping messages
// the harness wrote.
func lastUserText(stored []store.Message) string {
	for i := len(stored) - 1; i >= 0; i-- {
		if stored[i].Role != "user" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(stored[i].ContentJSON), &msg) == nil && !msg.Harness {
			return msg.Content
		}
	}
	return ""
}

// phaseSource says where the routed phase came from, so the shadow report can
// leave out turns whose phase Jev itself chose.
func phaseSource(state router.RoutingState) string {
	if state.InstructionPhase != nil {
		return "instruction:" + state.InstructionPhase.Source
	}
	return "declared"
}
