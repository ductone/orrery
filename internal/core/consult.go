package core

import (
	"context"
	"errors"
	"strings"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
)

// consultSystem frames a consulted model: a stronger model asked for judgement
// at a hard point, with the session so far as context.
const consultSystem = `You are a senior engineer consulted by a coding agent partway through a task. The conversation so far is the agent's own session: the request, what it has read and run, and what it has done. The agent has stopped at a decision it finds hard and asks you about it in the last message.

Answer that question directly. Give a recommendation, the reasoning behind it in brief, the assumptions or risks the agent should check, and the next concrete step. If the question cannot be settled from the evidence, say what would settle it. Do not write the implementation unless asked. Be concise: a few short paragraphs at most.`

// consultDescription is the consult tool's description.
const consultDescription = "Ask a stronger model for judgement at a hard decision: an ambiguous or conflicting requirement, a design choice that would be costly to reverse, evidence that contradicts itself, or an approach that has failed twice. It sees this session so far and your question, and returns advice. Do not use it for routine steps you can decide yourself."

// consultOutputCap bounds a consulted model's answer, reasoning included.
const consultOutputCap = 16_000

// consult answers a hard question with a frontier model at high effort, given
// the session so far. Planning happens here, on demand, instead of as a phase
// every task passes through.
func (e *Engine) consult(ctx context.Context, sid, question string, emit EmitFunc) (any, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("consult needs a question")
	}
	_, providers, policy, _, _ := e.runtimeSnapshot()
	if providers == nil || policy == nil {
		return nil, errors.New("no model is configured to consult")
	}
	s, err := e.store.Session(ctx, sid)
	if err != nil {
		return nil, err
	}
	history, err := e.providerMessages(ctx, sid)
	if err != nil {
		return nil, err
	}
	// The call that asked is still open: drop it, since a provider needs every
	// tool call answered and the question is restated below.
	if n := len(history); n > 0 && history[n-1].Role == "assistant" && len(history[n-1].ToolCalls) > 0 {
		history = history[:n-1]
	}
	history = append(history, provider.Message{Role: "user", Content: "QUESTION FROM THE AGENT\n" + question})
	state := router.RoutingState{SessionID: sid, Turn: s.Turn, Point: router.Escalation, Phase: router.Plan, TierPin: model.Frontier, EffortPin: model.EffortHigh,
		InputTokens: estimate(s.Spec) + estimate(question), EstimatedOutput: 4000, AvailableModels: providers.AvailableIDs(), Performance: e.routePerformance(ctx)}
	for _, m := range history {
		state.InputTokens += estimate(m.Content)
	}
	decision, _, err := policy.Decide(ctx, state)
	if err != nil {
		// No frontier model is allowed (a pinned default, say): consult the
		// best model routing does allow.
		state.TierPin = ""
		if decision, _, err = policy.Decide(ctx, state); err != nil {
			return nil, err
		}
	}
	resp, err := providers.CompleteOne(ctx, decision, func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
		return provider.Request{System: consultSystem, DurableSpec: durableSpec(s), Messages: history, MaxOutput: min(consultOutputCap, m.MaxOutput), Effort: d.Effort, NoToolCalls: true}, nil
	})
	if err != nil {
		return nil, err
	}
	cost := decision.Model.Pricing.EstimateDetailed(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
	_ = e.store.AddSpend(ctx, sid, cost)
	_ = e.store.RecordModelCall(ctx, decision.Model.ID, resp.Latency, resp.Usage.OutputTokens, resp.Truncated, resp.Usage.InputTokens, resp.Usage.CacheReadTokens)
	e.emit(ctx, sid, "usage.reported", map[string]any{"model": decision.Model.ID, "kind": "consult", "input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens, "cache_read_tokens": resp.Usage.CacheReadTokens, "cost_usd": cost, "latency": resp.Latency, "effort": decision.Effort}, emit)
	advice := strings.TrimSpace(resp.Message.Content)
	if advice == "" {
		return nil, errors.New("the consulted model returned no advice")
	}
	e.emit(ctx, sid, "consult.answered", map[string]any{"model": decision.Model.ID, "question": truncate(question, 2_000), "cost_usd": cost}, emit)
	return map[string]any{"advice": advice, "model": decision.Model.ID}, nil
}
