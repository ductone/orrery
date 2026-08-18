package core

import (
	"context"
	"crypto/sha256"
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
	Objective        string   `json:"objective"`
	CurrentObjective string   `json:"current_objective"`
	PendingReport    string   `json:"pending_report"`
	ResolvedRequests []string `json:"resolved_requests"`
	Requirements     []string `json:"requirements"`
	Decisions        []string `json:"decisions"`
	Completed        []string `json:"completed"`
	Files            []string `json:"files"`
	Verification     []string `json:"verification"`
	OpenWork         []string `json:"open_work"`
	Blockers         []string `json:"blockers"`
	Instructions     []string `json:"instructions"`
	WorkerResults    []string `json:"worker_results"`
	PriorSummary     string   `json:"prior_summary,omitempty"`
	CompactedAt      string   `json:"compacted_at"`
}

type durableAnchor struct {
	CurrentObjective string
	PendingReport    string
	Source           string
	TodoPosition     int
	TodoPhase        string
	TodoStatus       string
	TodoText         string
}

type resolvedRequestCounts struct {
	Prior    int
	Semantic int
	Detected int
	Final    int
}

func (a durableAnchor) hasTodo() bool { return strings.TrimSpace(a.TodoText) != "" }

func (d DurableState) valid() bool {
	return strings.TrimSpace(d.Objective) != "" && (len(d.Requirements) > 0 || len(d.Completed) > 0 || len(d.OpenWork) > 0 || len(d.Decisions) > 0 || len(d.Verification) > 0 || len(d.Blockers) > 0)
}

// Compact creates a recovery checkpoint before replacing history. Failure to
// obtain a semantic model summary degrades to a structured transcript digest;
// it never leaves the session half-compacted.
func (e *Engine) Compact(ctx context.Context, sid, reason string, emit EmitFunc) error {
	if !e.sessionIdle(sid) {
		return errors.New("session has an active turn")
	}
	return e.compactState(ctx, sid, reason, emit)
}

func (e *Engine) compactState(ctx context.Context, sid, reason string, emit EmitFunc) error {
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

	todos, err := e.store.Todos(ctx, sid)
	if err != nil {
		return err
	}
	cont, err := e.store.Continuation(ctx, sid)
	if err != nil {
		return err
	}
	workItems, err := e.store.WorkItems(ctx, sid)
	if err != nil {
		return err
	}
	state, meta, summaryErr := e.semanticSummary(ctx, s, todos, cont, workItems, msgs[:keepAt])
	if summaryErr != nil {
		state = fallbackDurableState(s, msgs[:keepAt])
		meta = map[string]any{"strategy": "structured_fallback", "error": summaryErr.Error()}
	}
	// The continuation ledger and todo plan are persistent session state,
	// whereas the semantic summary is best-effort model output. Keep the task
	// to continue and the report still owed deterministic so compaction cannot
	// resurrect an older answered prompt.
	state, anchor, resolvedCounts := anchorDurableState(state, s, todos, cont, workItems, msgs[:keepAt])
	if err := validateCompactionAnchor(state, anchor); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	s.DurableSummary = string(encoded)
	if err = e.store.ApplyCompaction(ctx, s, len(msgs)-keepAt); err != nil {
		return err
	}
	if err := e.mcpBoundary(ctx); err != nil {
		e.emit(ctx, sid, "runtime_config.reload_failed", map[string]any{"error": err.Error()}, emit)
	}
	for k, v := range anchorCompactionMeta(anchor, resolvedCounts) {
		meta[k] = v
	}
	meta["kept_messages"] = len(msgs) - keepAt
	meta["kept_turns"] = 4
	meta["checkpoint_id"] = cp.ID
	meta["reason"] = reason
	if meta["strategy"] == "semantic" {
		e.emit(ctx, sid, "usage.reported", map[string]any{"model": meta["model"], "kind": "compaction", "input_tokens": meta["input_tokens"], "output_tokens": meta["output_tokens"], "cost_usd": meta["cost_usd"]}, emit)
	}
	e.emit(ctx, sid, "context.compacted", meta, emit)
	return nil
}

func (e *Engine) compact(ctx context.Context, sid string, emit EmitFunc) {
	if err := e.compactState(ctx, sid, "phase_or_context_boundary", emit); err != nil {
		e.emit(ctx, sid, "context.compaction_failed", map[string]any{"error": err.Error()}, emit)
	}
}

func (e *Engine) semanticSummary(ctx context.Context, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, old []store.Message) (DurableState, map[string]any, error) {
	spec, ok := model.Get(s.Model)
	_, providers, _, _, _ := e.runtimeSnapshot()
	if !ok || providers == nil || !providers.Available(spec) {
		return DurableState{}, nil, errors.New("current model unavailable for semantic summary")
	}
	transcript := compactTranscript(old, 96_000)
	system := `Summarize an autonomous coding session for lossless continuation. Return one JSON object only with these exact keys: objective, current_objective, pending_report, resolved_requests, requirements, decisions, completed, files, verification, open_work, blockers, instructions, worker_results. objective, current_objective, and pending_report are strings; every other field is an array of concise strings. current_objective and pending_report must preserve the supplied LIVE TODO ANCHOR rather than an older request. resolved_requests lists requests already answered or superseded; never make them active again. Preserve concrete paths, symbols, commands, test outcomes, constraints, unresolved hypotheses, loaded instruction/skill names, and worker findings. Do not invent completion or evidence.`
	prompt := "ORIGINAL TASK\n" + s.Spec + "\n\nLIVE TODO ANCHOR (authoritative)\n" + durableTaskAnchor(s, todos, cont, workItems) + "\n\nPRIOR DURABLE STATE\n" + s.DurableSummary + "\n\nACTIVITY TO COMPACT\n" + transcript
	estimatedCost := spec.Pricing.Estimate(estimate(prompt), 2200, 0)
	if s.BudgetUSD > 0 && estimatedCost > s.BudgetUSD-s.SpentUSD {
		return DurableState{}, nil, errors.New("insufficient remaining budget for semantic summary")
	}
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

// anchorDurableState derives recovery-critical continuation state from the
// persisted todo plan. Unlike a transcript summary, todos survive compaction
// and therefore remain authoritative when the retained turns no longer show
// the active implementation work.
func anchorDurableState(state DurableState, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, old []store.Message) (DurableState, durableAnchor, resolvedRequestCounts) {
	anchor := selectDurableAnchor(s, todos, cont, workItems)
	prior := priorResolvedRequests(s.DurableSummary)
	semantic := state.ResolvedRequests
	detected := resolvedRequests(s, old)
	state.CurrentObjective = anchor.CurrentObjective
	state.PendingReport = anchor.PendingReport
	state.ResolvedRequests = mergeResolvedRequests(prior, semantic, detected)
	return state, anchor, resolvedRequestCounts{Prior: len(prior), Semantic: len(semantic), Detected: len(detected), Final: len(state.ResolvedRequests)}
}

func durableTaskAnchor(s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem) string {
	current, report := durableTaskAnchorParts(s, todos, cont, workItems)
	return "CURRENT OBJECTIVE\n" + current + "\n\nPENDING REPORT\n" + report
}

func durableTaskAnchorParts(s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem) (string, string) {
	anchor := selectDurableAnchor(s, todos, cont, workItems)
	return anchor.CurrentObjective, anchor.PendingReport
}

// selectDurableAnchor prefers the harness-owned continuation ledger when one
// exists: the ledger's active work item and report obligation are explicit
// state, not inference over mutable todo prose. Sessions that predate the
// ledger fall back to the todo heuristic.
func selectDurableAnchor(s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem) durableAnchor {
	if cont.SessionID != "" {
		if anchor, ok := ledgerActiveAnchor(s, cont, workItems); ok {
			return anchor
		}
		if anchor, ok := ledgerWrapUpAnchor(s, cont, workItems); ok {
			return anchor
		}
		return durableAnchor{
			CurrentObjective: fmt.Sprintf("Continue the current %s work for the original task.", s.Phase),
			PendingReport:    "Before final response, report the active work, verification, and any blockers. Do not reopen resolved requests.",
			Source:           "session_phase",
			TodoPosition:     -1,
		}
	}
	for _, status := range []string{"in_progress", "pending"} {
		for i, todo := range todos {
			text := strings.TrimSpace(todo.Text)
			if todo.Status == status && text != "" {
				phase := strings.TrimSpace(todo.Phase)
				if phase == "" {
					phase = s.Phase
				}
				return durableAnchor{
					CurrentObjective: fmt.Sprintf("Continue the active %s task: %s", phase, text),
					PendingReport:    "Complete the active todo before final response; then report the changes, verification, and any blockers. Do not reopen resolved requests.",
					Source:           status,
					TodoPosition:     i,
					TodoPhase:        phase,
					TodoStatus:       todo.Status,
					TodoText:         text,
				}
			}
		}
	}
	lastCompletedIndex := -1
	var lastCompleted store.Todo
	for i, todo := range todos {
		if todo.Status == "completed" && strings.TrimSpace(todo.Text) != "" {
			lastCompletedIndex = i
			lastCompleted = todo
		}
	}
	if lastCompletedIndex >= 0 {
		text := strings.TrimSpace(lastCompleted.Text)
		phase := strings.TrimSpace(lastCompleted.Phase)
		if phase == "" {
			phase = s.Phase
		}
		return durableAnchor{
			CurrentObjective: fmt.Sprintf("Wrap up the completed %s task: %s", phase, text),
			PendingReport:    fmt.Sprintf("The active plan is complete. Produce the final report for the completed todo %q, its files, verification, and blockers; do not re-answer resolved earlier questions.", text),
			Source:           "completed_wrap_up",
			TodoPosition:     lastCompletedIndex,
			TodoPhase:        phase,
			TodoStatus:       lastCompleted.Status,
			TodoText:         text,
		}
	}
	if len(todos) > 0 {
		return durableAnchor{
			CurrentObjective: "Wrap up the active task: report the completed plan and verification; do not revisit prior resolved requests.",
			PendingReport:    "The active plan is complete. Produce the final report for its completed work, files, verification, and blockers; do not re-answer resolved earlier questions.",
			Source:           "todo_wrap_up",
			TodoPosition:     -1,
		}
	}
	return durableAnchor{
		CurrentObjective: fmt.Sprintf("Continue the current %s work for the original task.", s.Phase),
		PendingReport:    "Before final response, report the active work, verification, and any blockers. Do not reopen resolved requests.",
		Source:           "session_phase",
		TodoPosition:     -1,
	}
}

func ledgerActiveAnchor(s store.Session, cont store.Continuation, workItems []store.WorkItem) (durableAnchor, bool) {
	if cont.ActiveWorkItemID == "" {
		return durableAnchor{}, false
	}
	for i, wi := range workItems {
		if wi.ID == cont.ActiveWorkItemID {
			phase := strings.TrimSpace(wi.Phase)
			if phase == "" {
				phase = s.Phase
			}
			return durableAnchor{
				CurrentObjective: fmt.Sprintf("Continue the active %s task: %s", phase, wi.Objective),
				PendingReport:    "Complete the active todo before final response; then report the changes, verification, and any blockers. Do not reopen resolved requests.",
				Source:           wi.Status,
				TodoPosition:     i,
				TodoPhase:        phase,
				TodoStatus:       wi.Status,
				TodoText:         wi.Objective,
			}, true
		}
	}
	return durableAnchor{}, false
}

func ledgerWrapUpAnchor(s store.Session, cont store.Continuation, workItems []store.WorkItem) (durableAnchor, bool) {
	if !cont.FinalReportRequired {
		return durableAnchor{}, false
	}
	lastIndex := -1
	var last store.WorkItem
	for i, wi := range workItems {
		if wi.Status == "completed" && strings.TrimSpace(wi.Objective) != "" && (lastIndex < 0 || wi.CompletedAt.After(last.CompletedAt) || (wi.CompletedAt.Equal(last.CompletedAt) && wi.Position > last.Position)) {
			lastIndex = i
			last = wi
		}
	}
	if lastIndex >= 0 {
		phase := strings.TrimSpace(last.Phase)
		if phase == "" {
			phase = s.Phase
		}
		return durableAnchor{
			CurrentObjective: fmt.Sprintf("Wrap up the completed %s task: %s", phase, last.Objective),
			PendingReport:    fmt.Sprintf("The active plan is complete. Produce the final report for the completed todo %q, its files, verification, and blockers; do not re-answer resolved earlier questions.", last.Objective),
			Source:           "completed_wrap_up",
			TodoPosition:     lastIndex,
			TodoPhase:        phase,
			TodoStatus:       last.Status,
			TodoText:         last.Objective,
		}, true
	}
	return durableAnchor{
		CurrentObjective: "Wrap up the active task: report the completed plan and verification; do not revisit prior resolved requests.",
		PendingReport:    "The active plan is complete. Produce the final report for its completed work, files, verification, and blockers; do not re-answer resolved earlier questions.",
		Source:           "todo_wrap_up",
		TodoPosition:     -1,
	}, true
}

func validateCompactionAnchor(state DurableState, anchor durableAnchor) error {
	if strings.TrimSpace(state.CurrentObjective) == "" {
		return errors.New("compaction anchor missing current objective")
	}
	if strings.TrimSpace(state.PendingReport) == "" {
		return errors.New("compaction anchor missing pending report")
	}
	switch anchor.Source {
	case "in_progress", "pending":
		if anchor.hasTodo() && !containsFold(state.CurrentObjective, anchor.TodoText) {
			return fmt.Errorf("compaction anchor current objective lost %s todo", anchor.Source)
		}
		// A resolved request must not be the active work. Only the todo text is
		// active work; the objective prefix and phase are boilerplate. Wrap-up
		// sources name completed work for reporting, which is not reopening it.
		for _, resolved := range state.ResolvedRequests {
			resolved = strings.TrimSpace(resolved)
			if resolved != "" && containsFold(anchor.TodoText, resolved) {
				return errors.New("compaction anchor reopened a resolved request as active work")
			}
		}
	case "completed_wrap_up":
		if anchor.hasTodo() && !containsFold(state.CurrentObjective+"\n"+state.PendingReport, anchor.TodoText) {
			return errors.New("compaction wrap-up anchor lost completed todo")
		}
	}
	return nil
}

func anchorCompactionMeta(anchor durableAnchor, counts resolvedRequestCounts) map[string]any {
	rendered := anchor.CurrentObjective + "\n\n" + anchor.PendingReport
	sum := sha256.Sum256([]byte(rendered))
	meta := map[string]any{
		"anchor_source":              anchor.Source,
		"anchor_hash":                fmt.Sprintf("%x", sum)[:16],
		"resolved_requests_prior":    counts.Prior,
		"resolved_requests_semantic": counts.Semantic,
		"resolved_requests_detected": counts.Detected,
		"resolved_requests_final":    counts.Final,
	}
	if anchor.TodoPosition >= 0 {
		meta["anchor_todo_position"] = anchor.TodoPosition
	}
	if strings.TrimSpace(anchor.TodoPhase) != "" {
		meta["anchor_todo_phase"] = anchor.TodoPhase
	}
	if strings.TrimSpace(anchor.TodoStatus) != "" {
		meta["anchor_todo_status"] = anchor.TodoStatus
	}
	return meta
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(strings.TrimSpace(needle)))
}

func priorResolvedRequests(summary string) []string {
	var state DurableState
	if json.Unmarshal([]byte(summary), &state) != nil {
		return nil
	}
	return state.ResolvedRequests
}

// resolvedRequests records only clearly answered question-like prompts. This
// is intentionally narrow: it prevents a direct old answer (for example a
// date lookup) from taking over after compaction without treating ordinary
// task instructions as resolved merely because the model discussed them.
func resolvedRequests(s store.Session, msgs []store.Message) []string {
	resolved := []string{}
	for i, message := range msgs {
		prompt := messageContent(message)
		if message.Role != "user" || !questionLike(prompt) || !answeredBeforeNextUser(msgs[i+1:]) {
			continue
		}
		resolved = appendBounded(resolved, truncate(strings.TrimSpace(prompt), 360), 12)
	}
	if questionLike(s.Spec) && answeredOriginalRequest(msgs) {
		resolved = appendBounded(resolved, truncate(strings.TrimSpace(s.Spec), 360), 12)
	}
	return resolved
}

func messageContent(message store.Message) string {
	var parsed provider.Message
	if json.Unmarshal([]byte(message.ContentJSON), &parsed) == nil && strings.TrimSpace(parsed.Content) != "" {
		return parsed.Content
	}
	return message.ContentJSON
}

func questionLike(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "?") || strings.HasPrefix(text, "what ") || strings.HasPrefix(text, "when ") || strings.HasPrefix(text, "where ") || strings.HasPrefix(text, "who ") || strings.HasPrefix(text, "why ") || strings.HasPrefix(text, "how ")
}

func answeredBeforeNextUser(msgs []store.Message) bool {
	for _, message := range msgs {
		if message.Role == "user" {
			return false
		}
		if message.Role != "assistant" {
			continue
		}
		var parsed provider.Message
		if json.Unmarshal([]byte(message.ContentJSON), &parsed) == nil && len(parsed.ToolCalls) == 0 && strings.TrimSpace(parsed.Content) != "" {
			return true
		}
	}
	return false
}

func answeredOriginalRequest(msgs []store.Message) bool {
	for _, message := range msgs {
		if message.Role == "user" {
			return false
		}
		if message.Role != "assistant" {
			continue
		}
		var parsed provider.Message
		if json.Unmarshal([]byte(message.ContentJSON), &parsed) == nil && len(parsed.ToolCalls) == 0 && strings.TrimSpace(parsed.Content) != "" {
			return true
		}
	}
	return false
}

func mergeResolvedRequests(groups ...[]string) []string {
	seen := map[string]bool{}
	merged := []string{}
	for _, group := range groups {
		for _, item := range group {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			merged = appendBounded(merged, item, 12)
		}
	}
	return merged
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
