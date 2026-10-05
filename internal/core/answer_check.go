package core

import (
	"context"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/store"
)

// A conversation that moves on can leave an agent answering an earlier
// request: the session's first message is the most durable text it has, and
// compaction removes the newer ones from history. With jev.review, Jev checks
// each final result against the latest request before verification and review
// run. The check fails open, and refuses at most maxAnswerRejections times.
const (
	maxAnswerRejections = 2
	// offTopicBelow is the probability under which a result counts as not
	// addressing the request. It is low: the check exists to catch answers
	// to a different question, not to grade thin ones.
	offTopicBelow      = 0.25
	answerCheckTimeout = 5 * time.Second
	answerCheckChars   = 6_000
)

var answerQuestion = map[string]jev.Question{"addresses_request": jev.Noul(
	"Does this final result address the person's latest request?",
	"It does the work or answers the question the latest request asks, even partly, or explains why it cannot be done.",
	"It answers a different question, such as an earlier request in the session, and does not engage with what the latest request asks.",
)}

// answerOffTopic reports whether a final result fails to address the latest
// request, returning the request it was checked against.
func (e *Engine) answerOffTopic(ctx context.Context, sid string, s store.Session, latest, result string, emit EmitFunc) (string, bool) {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	cfg.Jev = cfg.EffectiveJev()
	request := strings.TrimSpace(latest)
	if request == "" {
		request = strings.TrimSpace(s.Spec)
	}
	if !cfg.Jev.Review || cfg.Jev.APIKey == "" || request == "" || strings.TrimSpace(result) == "" {
		return request, false
	}
	state := map[string]any{"latest_request": truncate(request, answerCheckChars), "final_result": truncate(result, answerCheckChars)}
	// A request can point at work instead of describing it ("Implement bead
	// orrery-vau"); the todo plan is the request as the agent understood it.
	plan := []string{}
	if todos, err := e.store.Todos(ctx, sid); err == nil {
		for _, td := range todos {
			plan = append(plan, td.Text)
		}
	}
	if len(plan) > 0 {
		state["todo_plan"] = truncate(strings.Join(plan, "\n"), answerCheckChars)
	}
	askCtx, cancel := context.WithTimeout(ctx, answerCheckTimeout)
	defer cancel()
	resp, err := jev.New(cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Model, answerCheckTimeout).Ask(askCtx, state, answerQuestion)
	if err != nil {
		e.emit(ctx, sid, "completion.answer_check", map[string]any{"error": err.Error()}, emit)
		return request, false
	}
	a := resp.Answers["addresses_request"]
	if a.Noul == nil {
		return request, false
	}
	off := *a.Noul < offTopicBelow
	e.emit(ctx, sid, "completion.answer_check", map[string]any{"addresses_request": *a.Noul, "off_topic": off}, emit)
	return request, off
}
