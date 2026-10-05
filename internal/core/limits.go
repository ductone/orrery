package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// Limits change strategy; they never end a run. A run ends when the work is
// done, when it needs the person, or on an error nothing can recover from.
// A misbehaving model is excluded and the turn rerouted; budgets the person set
// pause and ask to continue. Headless runs exit at the question, resumable.

const (
	// limitQuestion and budgetQuestion prefix the ids of pending inputs the
	// harness raises, so their answers can be acted on.
	limitQuestion  = "limit-"
	budgetQuestion = "budget-"

	choiceContinue = "Keep going"
	choiceStop     = "Stop here"
)

// askToContinue pauses the run with a question for the person.
func (e *Engine) askToContinue(sid, kind, question string, choices []string, outcome agentproto.Outcome, emit EmitFunc) agentproto.TaskResult {
	input := agentproto.InputRequest{ID: kind + uuid.NewString(), Question: question, Choices: choices, AllowFreeform: true}
	ctx := context.Background()
	if err := e.store.CreatePendingInput(ctx, storePendingInput(sid, input)); err != nil {
		return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: question + " (and the question could not be recorded: " + err.Error() + ")"}, emit)
	}
	e.emit(ctx, sid, "limit.reached", map[string]any{"kind": strings.TrimSuffix(kind, "-"), "question": question}, emit)
	return e.pauseForInput(sid, input, outcome, emit)
}

// askAboutLimit asks whether to keep going after a limit or an error.
func (e *Engine) askAboutLimit(sid, question string, outcome agentproto.Outcome, emit EmitFunc) agentproto.TaskResult {
	return e.askToContinue(sid, limitQuestion, question, []string{choiceContinue, choiceStop}, outcome, emit)
}

// budgetChoice is the answer that extends the budget by extension dollars.
func budgetChoice(extension float64) string {
	return fmt.Sprintf("Add $%.2f and continue", extension)
}

// declines reports whether an answer to a harness question says to stop.
// Anything else, including free-form guidance, means continue.
func declines(answer string) bool {
	a := strings.ToLower(strings.TrimSpace(answer))
	for _, stop := range []string{strings.ToLower(choiceStop), "stop", "no", "cancel", "don't", "do not"} {
		if a == stop || strings.HasPrefix(a, stop+" ") || strings.HasPrefix(a, stop+".") || strings.HasPrefix(a, stop+",") {
			return true
		}
	}
	return false
}

// answeredLimit is a harness question the person answered with this run's
// message.
type answeredLimit struct {
	budget, declined bool
}

// limitAnswer reports whether the latest message answered a harness question.
func (e *Engine) limitAnswer(ctx context.Context, sid string) (answeredLimit, bool) {
	inputs, err := e.store.PendingInputs(ctx, sid)
	if err != nil || len(inputs) == 0 {
		return answeredLimit{}, false
	}
	last := inputs[len(inputs)-1]
	if last.Status != "answered" || !(strings.HasPrefix(last.ID, limitQuestion) || strings.HasPrefix(last.ID, budgetQuestion)) {
		return answeredLimit{}, false
	}
	latest, _ := e.store.LatestRequest(ctx, sid)
	if strings.TrimSpace(latest) != strings.TrimSpace(last.Answer) {
		return answeredLimit{}, false
	}
	return answeredLimit{budget: strings.HasPrefix(last.ID, budgetQuestion), declined: declines(last.Answer)}, true
}

func (e *Engine) harnessMessage(ctx context.Context, sid, text string) {
	_ = e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Harness: true, Content: text})
}

func storePendingInput(sid string, in agentproto.InputRequest) store.PendingInput {
	return store.PendingInput{ID: in.ID, SessionID: sid, Question: in.Question, Choices: in.Choices, AllowFreeform: in.AllowFreeform}
}

// routeFailure ends a turn that could not reach a model: no compatible model,
// or a provider error retrying cannot fix. A root session asks the person,
// who can retry once the cause is fixed; a worker fails, so its parent sees
// it and decides.
func (e *Engine) routeFailure(sid, parentJob string, err error, outcome agentproto.Outcome, emit EmitFunc) agentproto.TaskResult {
	if parentJob == "" {
		return e.askAboutLimit(sid, "I couldn't reach a model to continue: "+err.Error()+". Keep going to retry, or stop here.", outcome, emit)
	}
	return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: err.Error()}, emit)
}

// routeFailureAfter is routeFailure that also says which models this run set
// aside, and why, since that is usually what left nothing to route to.
func (e *Engine) routeFailureAfter(sid, parentJob string, err error, progress *progressTracker, outcome agentproto.Outcome, emit EmitFunc) agentproto.TaskResult {
	if len(progress.exclusionReasons) == 0 {
		return e.routeFailure(sid, parentJob, err, outcome, emit)
	}
	var parts []string
	for _, m := range progress.excluded {
		if r := progress.exclusionReasons[m]; r != "" {
			parts = append(parts, m+" ("+r+")")
		}
	}
	setAside := "models set aside this run for misbehaving: " + strings.Join(parts, "; ")
	if parentJob == "" {
		return e.askAboutLimit(sid, "I couldn't reach a model to continue ("+err.Error()+"); "+setAside+". Keep going to retry with every model, or stop here.", outcome, emit)
	}
	return e.finish(sid, agentproto.TaskResult{Status: agentproto.Fail, Outcome: outcome, Error: err.Error()}, emit)
}

// dropModel takes a model that keeps misbehaving (empty replies, truncated
// calls, results that fail the schema) out of the rest of the run. Another
// model usually does the work; when none is left, routing asks the person.
func (e *Engine) dropModel(ctx context.Context, sid, model, reason string, progress *progressTracker, emit EmitFunc) {
	progress.exclude(model)
	if progress.exclusionReasons == nil {
		progress.exclusionReasons = map[string]string{}
	}
	progress.exclusionReasons[model] = reason
	e.emit(ctx, sid, "routing.model_excluded", map[string]any{"model": model, "reason": reason}, emit)
}

// partialResult is what a worker stopped by its budget slice has found: its
// latest substantive message, so the parent can continue from it instead of
// starting over. It is nil when the worker said nothing yet.
func (e *Engine) partialResult(sid string) map[string]any {
	messages, err := e.store.Messages(context.Background(), sid)
	if err != nil {
		return nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(messages[i].ContentJSON), &msg) == nil && strings.TrimSpace(msg.Content) != "" {
			return map[string]any{"partial": true, "findings": truncate(msg.Content, 8_000)}
		}
	}
	return nil
}
