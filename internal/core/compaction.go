package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

// DurableState is the stable, machine-readable recovery contract produced by
// compaction. Fields are intentionally boring: a later model should be able to
// resume without reconstructing intent from prose fragments.
type DurableState struct {
	Objective     string   `json:"objective"`
	Requirements  []string `json:"requirements"`
	Decisions     []string `json:"decisions"`
	Completed     []string `json:"completed"`
	Files         []string `json:"files"`
	Verification  []string `json:"verification"`
	OpenWork      []string `json:"open_work"`
	Blockers      []string `json:"blockers"`
	Instructions  []string `json:"instructions"`
	WorkerResults []string `json:"worker_results"`
	PriorSummary  string   `json:"prior_summary,omitempty"`
	CompactedAt   string   `json:"compacted_at"`
}

func (d DurableState) valid() bool {
	return strings.TrimSpace(d.Objective) != "" && (len(d.Completed) > 0 || len(d.OpenWork) > 0 || len(d.Decisions) > 0)
}

// Compact creates a recovery checkpoint before replacing history. Failure to
// obtain a semantic model summary degrades to a structured transcript digest;
// it never leaves the session half-compacted.
func (e *Engine) Compact(ctx context.Context, sid, reason string, emit EmitFunc) error {
	msgs, err := e.store.Messages(ctx, sid)
	if err != nil {
		return err
	}
	keepAt := compactionKeepIndex(msgs, 4)
	if keepAt == len(msgs) || keepAt == 0 {
		return nil
	}
	s, err := e.store.Session(ctx, sid)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "manual"
	}
	cp, err := e.store.CreateCheckpoint(ctx, uuid.NewString(), sid, "Before compaction", reason)
	if err != nil {
		return fmt.Errorf("checkpoint before compaction: %w", err)
	}

	state, meta, summaryErr := e.semanticSummary(ctx, s, msgs[:keepAt])
	if summaryErr != nil {
		state = fallbackDurableState(s, msgs[:keepAt])
		meta = map[string]any{"strategy": "structured_fallback", "error": summaryErr.Error()}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	s.DurableSummary = string(encoded)
	if err = e.store.UpdateSession(ctx, s); err != nil {
		return err
	}
	if err = e.store.CompactMessages(ctx, sid, len(msgs)-keepAt); err != nil {
		_ = e.store.RestoreCheckpoint(ctx, sid, cp.ID)
		return err
	}
	if err = e.store.InvalidateCaches(ctx, sid); err != nil {
		return err
	}
	if err := e.mcpBoundary(ctx); err != nil {
		e.emit(ctx, sid, "runtime_config.reload_failed", map[string]any{"error": err.Error()}, emit)
	}
	meta["kept_messages"] = len(msgs) - keepAt
	meta["kept_turns"] = 4
	meta["checkpoint_id"] = cp.ID
	meta["reason"] = reason
	e.emit(ctx, sid, "context.compacted", meta, emit)
	return nil
}

func (e *Engine) compact(ctx context.Context, sid string, emit EmitFunc) {
	if err := e.Compact(ctx, sid, "phase_or_context_boundary", emit); err != nil {
		e.emit(ctx, sid, "context.compaction_failed", map[string]any{"error": err.Error()}, emit)
	}
}

func (e *Engine) semanticSummary(ctx context.Context, s store.Session, old []store.Message) (DurableState, map[string]any, error) {
	spec, ok := model.Get(s.Model)
	_, providers, _, _, _ := e.runtimeSnapshot()
	if !ok || providers == nil || !providers.Available(spec) {
		return DurableState{}, nil, errors.New("current model unavailable for semantic summary")
	}
	transcript := compactTranscript(old, 96_000)
	system := `Summarize an autonomous coding session for lossless continuation. Return one JSON object only with these exact keys: objective, requirements, decisions, completed, files, verification, open_work, blockers, instructions, worker_results. objective is a string; every other field is an array of concise strings. Preserve concrete paths, symbols, commands, test outcomes, constraints, unresolved hypotheses, loaded instruction/skill names, and worker findings. Do not invent completion or evidence.`
	prompt := "ORIGINAL TASK\n" + s.Spec + "\n\nPRIOR DURABLE STATE\n" + s.DurableSummary + "\n\nACTIVITY TO COMPACT\n" + transcript
	effort := model.EffortLow
	if len(spec.Effort) > 0 {
		effort = spec.Effort[0]
		for _, candidate := range spec.Effort {
			if candidate == model.EffortLow {
				effort = candidate
			}
		}
	}
	decision := router.Decision{Model: spec, Effort: effort, EditDialect: spec.EditDialect, ToolsetVariant: "portable"}
	resp, err := providers.CompleteOne(ctx, decision, func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
		return provider.Request{System: system, DurableSpec: "Compaction is an internal state transition.", Messages: []provider.Message{{Role: "user", Content: prompt}}, MaxOutput: min(2200, m.MaxOutput), Effort: d.Effort, Strict: m.Compat.SupportsStrictTools}, nil
	})
	if err != nil {
		return DurableState{}, nil, err
	}
	var state DurableState
	content := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(resp.Message.Content), "```json"), "```"))
	if err := json.Unmarshal([]byte(content), &state); err != nil {
		return DurableState{}, nil, fmt.Errorf("decode semantic summary: %w", err)
	}
	state.PriorSummary = truncate(s.DurableSummary, 4000)
	state.CompactedAt = time.Now().UTC().Format(time.RFC3339)
	if !state.valid() {
		return DurableState{}, nil, errors.New("semantic summary omitted recovery state")
	}
	cost := spec.Pricing.EstimateDetailed(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
	_ = e.store.AddSpend(ctx, s.ID, cost)
	return state, map[string]any{"strategy": "semantic", "model": spec.ID, "input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens, "cost_usd": cost}, nil
}

func fallbackDurableState(s store.Session, old []store.Message) DurableState {
	completed, open, decisions, verification, files, instructions, workers := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	for _, m := range old {
		line := m.Role + ": " + truncate(strings.Join(strings.Fields(m.ContentJSON), " "), 900)
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "workspace_instructions") || strings.Contains(lower, "skill"):
			instructions = appendBounded(instructions, line, 8)
		case strings.Contains(lower, "job") && (strings.Contains(lower, "result") || strings.Contains(lower, "worker")):
			workers = appendBounded(workers, line, 8)
		case strings.Contains(lower, "test") || strings.Contains(lower, "go test") || strings.Contains(lower, "verified"):
			verification = appendBounded(verification, line, 10)
		case strings.Contains(lower, "edit") || strings.Contains(lower, "patch"):
			files = appendBounded(files, line, 12)
		case m.Role == "assistant":
			decisions = appendBounded(decisions, line, 12)
		default:
			open = appendBounded(open, line, 12)
		}
	}
	return DurableState{Objective: s.Spec, Decisions: decisions, Completed: completed, Files: files, Verification: verification, OpenWork: open, Instructions: instructions, WorkerResults: workers, PriorSummary: truncate(s.DurableSummary, 4000), CompactedAt: time.Now().UTC().Format(time.RFC3339)}
}

func appendBounded(items []string, value string, limit int) []string {
	if len(items) >= limit {
		return items
	}
	return append(items, value)
}

func compactTranscript(msgs []store.Message, limit int) string {
	var b strings.Builder
	for _, m := range msgs {
		line := m.Role + ": " + m.ContentJSON + "\n"
		if b.Len()+len(line) > limit {
			remaining := limit - b.Len()
			if remaining > 0 {
				b.WriteString(line[:min(remaining, len(line))])
			}
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
