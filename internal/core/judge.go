package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
)

// judgeMaxOutput bounds the verdict response. The judge answers a yes/no
// question with a short reason, so it never needs room to reason at length.
const judgeMaxOutput = 300

// judgeDigestTurns is how many recent assistant turns are summarised for the
// judge. Enough to see a loop, small enough to keep the call cheap.
const judgeDigestTurns = 8

const judgePrompt = `You decide whether an autonomous coding agent is genuinely stuck.

You are given counter signals and a digest of the agent's recent tool calls. The
counters are deliberately over-sensitive: they increment on any turn that did not
change a todo, spawn a worker, edit a file, or run a verification command. Turns
spent reading and searching therefore look identical to turns spent looping, even
when the agent is making steady progress.

Judge the behaviour, not the counters. The agent is NOT stuck if its recent calls
are gathering new information that plausibly advances the task: reading different
files, searching different terms, narrowing toward a location. The agent IS stuck
if it repeats equivalent calls, re-reads what it already has, oscillates between
the same few actions, or shows no path from its recent activity to the task.

Reply with JSON only: {"intervene": <bool>, "reason": "<one short sentence>"}
Set intervene true only when the agent is genuinely stuck and the intervention
would help. When the evidence is ambiguous, answer false.`

// judgeVerdict is the judge's answer. A nil verdict means the judge could not
// answer and the caller must fail cheap.
type judgeVerdict struct {
	Intervene bool   `json:"intervene"`
	Reason    string `json:"reason"`
}

// judgeIntervention asks a cheap model whether a tripped counter reflects a real
// stall. It reports (verdict, ok); ok is false when the judge was unavailable,
// timed out, or returned something unparseable, in which case the caller must
// neither intervene nor back off — a judge outage must not silently
// desensitise the session.
//
// The judge deliberately bypasses the router: it builds its own decision from
// the cheapest configured model so it is never subject to frontier_floor_phases.
func (e *Engine) judgeIntervention(ctx context.Context, sid, kind string, signals map[string]int, emit EmitFunc) (judgeVerdict, bool) {
	cfg, registry, _, _, _ := e.runtimeSnapshot()
	if !cfg.Interventions.Enabled() {
		// Judging disabled: preserve the pre-cascade behaviour, where a tripped
		// counter acts on its own.
		return judgeVerdict{Intervene: true, Reason: "judge disabled"}, true
	}
	m, ok := e.judgeModel(cfg.Interventions.JudgeModel, registry)
	if !ok {
		return judgeVerdict{}, false
	}
	digest := e.recentActivityDigest(ctx, sid)
	if strings.TrimSpace(digest) == "" {
		// No recorded activity to judge; fail cheap rather than guess.
		return judgeVerdict{}, false
	}
	spec := ""
	if s, err := e.store.Session(ctx, sid); err == nil {
		spec = s.Spec
	}
	content := fmt.Sprintf("Task:\n%s\n\nProposed intervention: %s\n\nCounter signals:\n%s\n\nRecent tool calls (oldest first):\n%s",
		spec, kind, formatSignals(signals), digest)

	ctx, cancel := context.WithTimeout(ctx, cfg.Interventions.JudgeTimeout())
	defer cancel()
	decision := router.Decision{Model: m, Effort: model.EffortLow}
	build := func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
		return provider.Request{
			System:    judgePrompt,
			Messages:  []provider.Message{{Role: "user", Content: content}},
			MaxOutput: min(judgeMaxOutput, m.MaxOutput),
			Effort:    d.Effort,
		}, nil
	}
	start := time.Now()
	resp, err := registry.CompleteOne(ctx, decision, build)
	if err != nil {
		e.emit(ctx, sid, "progress.judge", map[string]any{"kind": kind, "signals": signals, "model": m.ID, "error": err.Error()}, emit)
		return judgeVerdict{}, false
	}
	cost := m.Pricing.EstimateDetailed(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
	// The judge spends real money on the session's behalf, so it belongs in the
	// session's ledger rather than being invisible overhead.
	_ = e.store.AddSpend(ctx, sid, cost)
	verdict, parsed := parseJudgeVerdict(resp.Message.Content)
	payload := map[string]any{
		"kind": kind, "signals": signals, "model": m.ID,
		"cost_usd": cost, "latency": time.Since(start),
	}
	if !parsed {
		payload["error"] = "unparseable verdict"
		e.emit(ctx, sid, "progress.judge", payload, emit)
		return judgeVerdict{}, false
	}
	payload["intervene"] = verdict.Intervene
	payload["reason"] = verdict.Reason
	e.emit(ctx, sid, "progress.judge", payload, emit)
	return verdict, true
}

// judgeModel resolves the configured judge model, falling back to the cheapest
// configured one. A pinned model that is not configured is not silently
// swapped: the caller fails cheap instead of paying frontier rates by accident.
func (e *Engine) judgeModel(pinned string, registry *provider.Registry) (model.ModelSpec, bool) {
	if pinned != "" {
		m, ok := model.Get(pinned)
		if !ok || !registry.Available(m) {
			return model.ModelSpec{}, false
		}
		return m, true
	}
	m := cheapestAvailableModel(registry)
	return m, m.ID != ""
}

// parseJudgeVerdict extracts the verdict from a model response, tolerating the
// code fences small models often add.
func parseJudgeVerdict(content string) (judgeVerdict, bool) {
	text := strings.TrimSpace(content)
	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			text = text[start : end+1]
		}
	}
	var v judgeVerdict
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return judgeVerdict{}, false
	}
	return v, true
}

func formatSignals(signals map[string]int) string {
	keys := make([]string, 0, len(signals))
	for k := range signals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("  %s: %d", k, signals[k]))
	}
	return strings.Join(lines, "\n")
}

// recentActivityDigest renders the session's recent tool calls for the judge.
// It deliberately carries the calls themselves rather than the agent's prose:
// an agent that has lost the thread narrates confidently, and a judge reading
// that narration tends to agree with it.
func (e *Engine) recentActivityDigest(ctx context.Context, sid string) string {
	messages, err := e.store.Messages(ctx, sid)
	if err != nil {
		return ""
	}
	var turns []string
	for _, stored := range messages {
		if stored.Role != "assistant" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(stored.ContentJSON), &msg) != nil || len(msg.ToolCalls) == 0 {
			continue
		}
		calls := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			calls = append(calls, call.Name+"("+summariseArgs(call.Arguments)+")")
		}
		turns = append(turns, strings.Join(calls, ", "))
	}
	if len(turns) > judgeDigestTurns {
		turns = turns[len(turns)-judgeDigestTurns:]
	}
	lines := make([]string, 0, len(turns))
	for i, turn := range turns {
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, turn))
	}
	return strings.Join(lines, "\n")
}

// summariseArgs renders tool arguments compactly. Values are truncated because
// the judge needs to see which call was made, not its full payload.
func summariseArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		value := fmt.Sprint(args[k])
		if len(value) > 80 {
			value = value[:80] + "…"
		}
		parts = append(parts, k+"="+value)
	}
	return strings.Join(parts, " ")
}

// allowIntervention decides whether a tripped counter should actually act. The
// counter is only a trigger: it is deliberately over-sensitive, so the judge
// makes the real call. A declined intervention raises the signal's floor, so
// the same question is not re-asked on every subsequent turn of the phase.
//
// Every outcome fails cheap. An unavailable or unparseable judge returns false
// (do not act) without raising the floor, so an outage neither burns money on
// spurious interventions nor permanently desensitises the session.
func (e *Engine) allowIntervention(ctx context.Context, sid, kind, signal string, observed int, progress *progressTracker, emit EmitFunc) bool {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	// The floor is a memoised "not stuck" verdict from earlier in this phase:
	// until the signal exceeds it, the judge has already answered this question.
	if observed < progress.floors[signal] {
		return false
	}
	verdict, ok := e.judgeIntervention(ctx, sid, kind, progress.stall(), emit)
	if !ok {
		return false
	}
	if !verdict.Intervene {
		progress.backoff(signal, observed, cfg.Interventions.Backoff())
		return false
	}
	return true
}

// escalationTrigger reports which stall clause tripped, if any. Escalation is a
// disjunction over several signals, so backing off has to name the clause that
// actually fired rather than desensitising all of them at once.
func escalationTrigger(stall router.StallSignals, progress *progressTracker) (string, int, bool) {
	clauses := []struct {
		signal   string
		observed int
		base     int
	}{
		{"failed_commands", stall.FailedCommands, 2},
		{"test_fail_streak", stall.TestFailStreak, 2},
		{"repeated_edits", stall.RepeatedEdits, 3},
		{"no_progress_turns", stall.NoProgressTurns, 4},
		{"repeated_reads", stall.RepeatedReads, 2},
		{"repeated_searches", stall.RepeatedSearches, 2},
	}
	for _, c := range clauses {
		if c.observed >= progress.threshold(c.signal, c.base) {
			return c.signal, c.observed, true
		}
	}
	return "", 0, false
}
