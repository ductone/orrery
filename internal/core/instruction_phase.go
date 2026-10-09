package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

// A new user message has always been routed as planning: frontier-floored and
// at high effort. That is right for a new request and wasteful for "keep
// going" or "also fix the typo". With jev.routing, Jev reads the message
// against the current plan and chooses the phase; below the confidence bar the
// turn is planned, as before. The choice is stored as the session phase; from
// the next turn the agent's own todo plan decides again.
const (
	instructionPhaseVersion    = "instruction-phase/v2"
	previousAnswerChars        = 3_000
	instructionPhaseConfidence = 0.8
	instructionPhaseTimeout    = 3 * time.Second
)

var instructionPhaseQuestion = map[string]jev.Question{"phase": jev.Choice(
	"A person just sent this message to a coding agent partway through a session. Which phase of work should the agent's next turn be in?",
	map[string]string{
		"plan":      "The message asks for new or substantially different work, or changes the goal, so an approach is needed before changing anything.",
		"explore":   "The message asks a question, asks for a recommendation or explanation, or asks the agent to find or investigate something before acting.",
		"implement": "The message asks for a specific, well-defined change, or tells the agent to continue the implementation already planned.",
		"diagnose":  "The message reports a failure, error, or unexpected behaviour to investigate.",
		"review":    "The message asks the agent to check, test, or verify work already done.",
		"wrap-up":   "The message acknowledges the work, asks for a summary, or asks the agent to finish.",
	},
)}

// endsWithUserInstruction reports whether history ends with a message a
// person sent. Messages the harness wrote are not instructions.
func endsWithUserInstruction(stored []store.Message) bool {
	if len(stored) == 0 || stored[len(stored)-1].Role != "user" {
		return false
	}
	var msg provider.Message
	if json.Unmarshal([]byte(stored[len(stored)-1].ContentJSON), &msg) != nil {
		return true
	}
	return !msg.Harness
}

// instructionPhase chooses the phase for a turn that starts with a new user
// message.
func (e *Engine) instructionPhase(ctx context.Context, s store.Session, stored []store.Message, emit EmitFunc) *router.InstructionPhase {
	choice := &router.InstructionPhase{Phase: router.Plan, Source: "default", QuestionVersion: instructionPhaseVersion}
	cfg, _, _, _, _ := e.runtimeSnapshot()
	cfg.Jev = cfg.EffectiveJev()
	if !cfg.Jev.Routing || cfg.Jev.APIKey == "" {
		return choice
	}
	todos, _ := e.store.Todos(ctx, s.ID)
	plan := make([]map[string]string, 0, len(todos))
	for _, t := range todos {
		plan = append(plan, map[string]string{"text": t.Text, "phase": t.Phase, "status": t.Status})
	}
	state := map[string]any{
		"message":         truncate(lastUserText(stored), shadowSpecChars),
		"first_request":   truncate(s.Spec, shadowSpecChars),
		"current_phase":   s.Phase,
		"previous_plan":   plan,
		"previous_answer": truncate(lastAssistantText(stored), previousAnswerChars),
	}
	askCtx, cancel := context.WithTimeout(ctx, instructionPhaseTimeout)
	defer cancel()
	resp, err := jev.New(cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Model, instructionPhaseTimeout).Ask(askCtx, state, instructionPhaseQuestion)
	if err != nil {
		choice.Error = err.Error()
	} else if a := resp.Answers["phase"]; a.Choice != "" {
		choice.Suggested = router.Phase(a.Choice)
		if a.Confidence != nil {
			choice.Confidence = *a.Confidence
		}
		if choice.Confidence >= instructionPhaseConfidence {
			choice.Phase, choice.Source = choice.Suggested, "jev"
		}
	}
	e.emit(ctx, s.ID, "routing.instruction_phase", choice, emit)
	return choice
}
