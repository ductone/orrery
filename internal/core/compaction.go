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

	"github.com/ductone/orrey/internal/jev"
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
	// SchemaVersion identifies the DurableState shape so a future format
	// change can be detected before it is trusted as a recovery anchor.
	SchemaVersion int `json:"schema_version,omitempty"`
	// InputFromMessage/InputToMessage are the 0-based message indices (within
	// the session's full stored history at compaction time) that this
	// checkpoint summarized, for provenance and replay.
	InputFromMessage int `json:"input_from_message,omitempty"`
	InputToMessage   int `json:"input_to_message,omitempty"`
	// EvidenceRefs are bounded pointers (session/event or repository path) a
	// later turn can follow instead of re-deriving evidence from prose.
	EvidenceRefs     []string          `json:"evidence_refs,omitempty"`
	MemoryCandidates []MemoryCandidate `json:"memory_candidates,omitempty"`
	// Trigger/Reason record why compaction ran (phase_or_context_boundary,
	// manual, token_pressure, ...).
	Trigger string `json:"trigger,omitempty"`
	// InputTokenEstimate/OutputTokenEstimate are rough token estimates for the
	// summarized input and produced checkpoint, for benefit accounting.
	InputTokenEstimate  int `json:"input_token_estimate,omitempty"`
	OutputTokenEstimate int `json:"output_token_estimate,omitempty"`
	// ClearedToolBytes is how many bytes of stale tool output were cleared
	// (pointer/hash retained) rather than paraphrased, ahead of folding the
	// rest into this checkpoint.
	ClearedToolBytes int `json:"cleared_tool_bytes,omitempty"`
	// EvidenceCount is len(EvidenceRefs), carried redundantly so a consumer
	// need not re-derive it.
	EvidenceCount int `json:"evidence_count,omitempty"`
	// Validated reports whether validateCompactionAnchor accepted this state.
	// A checkpoint is only ever persisted after passing, so this is always
	// true for a state read back from a session's durable summary; it exists
	// so validation outcome travels with the checkpoint schema itself.
	Validated bool `json:"validated"`
	// CacheBoundaryID identifies the cache epoch this compaction started.
	// Memory selection pins its set to the same identifier, so a refresh at
	// session start, phase transition, or compaction can be correlated with
	// the checkpoint that caused it.
	CacheBoundaryID string `json:"cache_boundary_id,omitempty"`
}

// MemoryCandidate is an evidence-backed proposal extracted from a completed
// request or compaction, not a record of task progress.
type MemoryCandidate struct {
	Kind         string   `json:"kind"`
	Text         string   `json:"text"`
	EvidenceRefs []string `json:"evidence_refs"`
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
	// ActiveDropped counts resolved entries removed because they named the
	// active request.
	ActiveDropped int
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
	clearedBytes := clearedToolBytes(msgs[:keepAt])
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
	state = auditDurableState(state, msgs[:keepAt])
	// The person's latest request is the work in progress. A summary that
	// files it as resolved (as one did: "User explicitly requested: Build
	// it.") leaves the oldest question as the only open request in view.
	if latest, _ := e.store.LatestRequest(ctx, sid); latest != "" {
		before := len(state.ResolvedRequests)
		state.ResolvedRequests = withoutRequest(state.ResolvedRequests, latest)
		resolvedCounts.Final = len(state.ResolvedRequests)
		resolvedCounts.ActiveDropped = before - len(state.ResolvedRequests)
	}
	if err := validateCompactionAnchor(state, anchor); err != nil {
		return err
	}
	state.SchemaVersion = compactionStateSchemaVersion
	state.InputFromMessage = 0
	state.InputToMessage = keepAt - 1
	state.Trigger = reason
	state.InputTokenEstimate = estimate(messagesText(msgs[:keepAt]))
	state.EvidenceRefs = compactionEvidenceRefs(sid, state, keepAt)
	state.EvidenceCount = len(state.EvidenceRefs)
	state.ClearedToolBytes = clearedBytes
	state.Validated = true
	state.CacheBoundaryID = uuid.NewString()
	compactedEvents, err := e.store.EventsAfter(ctx, sid, 0)
	if err != nil {
		return err
	}
	compactedSeq := 0
	for _, event := range compactedEvents {
		if !event.CreatedAt.After(msgs[keepAt-1].CreatedAt) {
			compactedSeq = event.Seq
		}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	s.DurableSummary = string(encoded)
	if err = e.store.ApplyCompaction(ctx, s, len(msgs)-keepAt); err != nil {
		return err
	}
	if len(state.MemoryCandidates) > 0 {
		if err := e.persistMemoryCandidates(ctx, sid, s.WorkspacePath, state.MemoryCandidates, state.EvidenceRefs); err != nil {
			return err
		}
	}
	if summaryErr == nil {
		if err := e.store.AdvanceMemoryWatermark(ctx, sid, compactedSeq); err != nil {
			return err
		}
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
	meta["schema_version"] = state.SchemaVersion
	meta["input_from_message"] = state.InputFromMessage
	meta["input_to_message"] = state.InputToMessage
	meta["trigger"] = state.Trigger
	meta["input_token_estimate"] = state.InputTokenEstimate
	meta["cleared_tool_bytes"] = state.ClearedToolBytes
	meta["evidence_count"] = state.EvidenceCount
	meta["validated"] = state.Validated
	meta["cache_boundary_id"] = state.CacheBoundaryID
	meta["memory_cache_boundary_reason"] = memoryBoundaryCompaction
	e.setCacheBoundary(sid, state.CacheBoundaryID)
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

func (e *Engine) foldSummaries(ctx context.Context, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, chunks []string) (DurableState, map[string]any, error) {
	var state DurableState
	meta := map[string]any{}
	prior := s.DurableSummary
	for _, chunk := range chunks {
		s.DurableSummary = prior
		next, nextMeta, err := e.summarizeTranscript(ctx, s, todos, cont, workItems, chunk)
		if err != nil {
			return DurableState{}, nil, err
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return DurableState{}, nil, err
		}
		state, meta, prior = next, nextMeta, string(encoded)
	}
	return state, meta, nil
}

func (e *Engine) semanticSummary(ctx context.Context, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, old []store.Message) (DurableState, map[string]any, error) {
	_, registry, _, _, _ := e.runtimeSnapshot()
	cheap := sideModel(registry, model.Efficient)
	if cheap.ID != "" && cheap.ID != s.Model && !cheap.Discovered && tierAtLeast(cheap.Tier, model.Efficient) {
		cheapSession := s
		cheapSession.Model = cheap.ID
		state, meta, err := e.semanticSummaryWithModel(ctx, cheapSession, todos, cont, workItems, old)
		if err == nil {
			return state, meta, nil
		}
		// Failed summaries still consume tokens. Refresh spend before the fallback.
		if current, loadErr := e.store.Session(ctx, s.ID); loadErr == nil {
			s.SpentUSD = current.SpentUSD
		}
	}
	return e.semanticSummaryWithModel(ctx, s, todos, cont, workItems, old)
}

func (e *Engine) semanticSummaryWithModel(ctx context.Context, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, old []store.Message) (DurableState, map[string]any, error) {
	spec, ok := model.Get(s.Model)
	_, providers, _, _, _ := e.runtimeSnapshot()
	if !ok || providers == nil || !providers.Available(spec) {
		return DurableState{}, nil, errors.New("current model unavailable for semantic summary")
	}
	budget := summaryTranscriptBudget(spec)
	chunks := transcriptChunks(old, budget)
	if len(chunks) > 1 {
		return e.foldSummaries(ctx, s, todos, cont, workItems, chunks)
	}
	transcript := ""
	if len(chunks) == 1 {
		transcript = chunks[0]
	}
	return e.summarizeTranscript(ctx, s, todos, cont, workItems, transcript)
}

func (e *Engine) summarizeTranscript(ctx context.Context, s store.Session, todos []store.Todo, cont store.Continuation, workItems []store.WorkItem, transcript string) (DurableState, map[string]any, error) {
	spec, ok := model.Get(s.Model)
	_, providers, _, _, _ := e.runtimeSnapshot()
	if !ok || providers == nil || !providers.Available(spec) {
		return DurableState{}, nil, errors.New("current model unavailable for semantic summary")
	}
	system := `Summarize an autonomous coding session for lossless continuation. Return one JSON object only with these exact keys: objective, current_objective, pending_report, resolved_requests, requirements, decisions, completed, files, verification, open_work, blockers, instructions, worker_results, memory_candidates. objective, current_objective, and pending_report are strings; other fields are arrays of concise strings except memory_candidates, an optional array of objects with kind (fact, command, decision, preference, lesson), text, and evidence_refs (session/event pointers or repository paths, never copied content). Extract zero to a few evidence-backed facts true of the repository or person beyond this task, not session progress, secrets or speculation. current_objective and pending_report must preserve the supplied LIVE TODO ANCHOR rather than an older request. resolved_requests lists requests already answered or superseded; never make them active again. Preserve concrete paths, symbols, commands, test outcomes, constraints, unresolved hypotheses, loaded instruction/skill names, and worker findings. Do not invent completion or evidence.`
	active := ""
	if latest, _ := e.store.LatestRequest(ctx, s.ID); latest != "" && strings.TrimSpace(latest) != strings.TrimSpace(s.Spec) {
		active = "\n\nACTIVE REQUEST (the person's latest message; it is the work in progress and must never be listed in resolved_requests)\n" + latest
	}
	prompt := "ORIGINAL TASK\n" + s.Spec + active + "\n\nLIVE TODO ANCHOR (authoritative)\n" + durableTaskAnchor(s, todos, cont, workItems) + "\n\nPRIOR DURABLE STATE\n" + s.DurableSummary + "\n\nACTIVITY TO COMPACT\n" + transcript
	estimatedCost := spec.Pricing.Estimate(estimate(prompt), summaryOutputLimits[0], 0)
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
	// A summary cut off at its output limit is unparseable JSON, and a
	// reasoning model can spend much of a small limit before writing any.
	// Retry once with more room; every attempt is charged to the session.
	var resp provider.Response
	var state DurableState
	var cost float64
	for attempt, limit := range summaryOutputLimits {
		var err error
		resp, err = providers.CompleteOne(ctx, decision, func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
			return provider.Request{System: system, DurableSpec: "Compaction is an internal state transition.", Messages: []provider.Message{{Role: "user", Content: prompt}}, MaxOutput: min(limit, m.MaxOutput), Effort: d.Effort, Strict: m.Compat.SupportsStrictTools}, nil
		})
		if err != nil {
			return DurableState{}, nil, err
		}
		attemptCost := spec.Pricing.EstimateDetailed(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
		cost += attemptCost
		_ = e.store.AddSpend(ctx, s.ID, attemptCost)
		content := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(resp.Message.Content), "```json"), "```"))
		err = json.Unmarshal([]byte(content), &state)
		if err == nil {
			break
		}
		if !resp.Truncated || attempt == len(summaryOutputLimits)-1 {
			return DurableState{}, nil, fmt.Errorf("decode semantic summary (stop reason %q): %w", resp.StopReason, err)
		}
	}
	state.PriorSummary = truncate(s.DurableSummary, 4000)
	state.CompactedAt = time.Now().UTC().Format(time.RFC3339)
	if !state.valid() {
		return DurableState{}, nil, errors.New("semantic summary omitted recovery state")
	}
	return state, map[string]any{"strategy": "semantic", "model": spec.ID, "input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens, "cost_usd": cost}, nil
}

// summaryOutputLimits are the output limits of successive summary attempts.
var summaryOutputLimits = []int{4_000, 12_000}

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
		"anchor_source":                    anchor.Source,
		"anchor_hash":                      fmt.Sprintf("%x", sum)[:16],
		"resolved_requests_prior":          counts.Prior,
		"resolved_requests_semantic":       counts.Semantic,
		"resolved_requests_detected":       counts.Detected,
		"resolved_requests_final":          counts.Final,
		"resolved_requests_active_dropped": counts.ActiveDropped,
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

const (
	transcriptArgChars  = 500
	defaultToolHead     = 1_500
	editToolHead        = 4_000
	testToolHead        = 4_000
	readToolHead        = 400
	minTranscriptBudget = 8_000
)

func summaryTranscriptBudget(spec model.ModelSpec) int {
	if spec.ContextWindow <= 0 {
		return 96_000
	}
	return max(spec.ContextWindow/2, minTranscriptBudget)
}

func compactTranscript(msgs []store.Message, limit int) string {
	chunks := transcriptChunks(msgs, limit)
	if len(chunks) == 0 {
		return ""
	}
	return chunks[0]
}

func transcriptChunks(msgs []store.Message, limit int) []string {
	if limit < 1 {
		limit = minTranscriptBudget
	}
	lines := projectTranscript(msgs)
	var chunks []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		chunks = append(chunks, b.String())
		b.Reset()
	}
	for _, line := range lines {
		for len(line) > limit {
			flush()
			chunks = append(chunks, line[:limit])
			line = line[limit:]
		}
		if line == "" {
			continue
		}
		extra := len(line)
		if b.Len() > 0 {
			extra++
		}
		if b.Len() > 0 && b.Len()+extra > limit {
			flush()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	flush()
	return chunks
}

func projectTranscript(msgs []store.Message) []string {
	lines := make([]string, 0, len(msgs))
	for i, m := range msgs {
		var parsed provider.Message
		if json.Unmarshal([]byte(m.ContentJSON), &parsed) != nil {
			parsed = provider.Message{Role: m.Role, Content: m.ContentJSON}
		}
		switch m.Role {
		case "user":
			lines = append(lines, "user: "+parsed.Content)
		case "assistant":
			lines = append(lines, projectAssistant(parsed))
		default:
			name, args := matchingCall(msgs, i, parsed.ToolCallID)
			lines = append(lines, projectTool(name, args, parsed.Content))
		}
	}
	return lines
}

func projectAssistant(msg provider.Message) string {
	parts := []string{"assistant: " + truncate(strings.Join(strings.Fields(msg.Content), " "), transcriptArgChars)}
	for _, call := range msg.ToolCalls {
		args, _ := json.Marshal(call.Arguments)
		parts = append(parts, "call "+call.Name+" "+truncate(string(args), transcriptArgChars))
	}
	return strings.Join(parts, " | ")
}

func projectTool(name, args, content string) string {
	head := defaultToolHead
	switch {
	case name == "edit":
		head = editToolHead
	case name == "read" || name == "fetch" || name == "search":
		head = readToolHead
	case name == "exec" && (strings.Contains(args, "test") || strings.Contains(args, "build") || strings.Contains(args, "vet")):
		head = testToolHead
	}
	return "tool " + name + " args=" + truncate(args, transcriptArgChars) + " outcome=" + toolOutcome(content) + " head=" + truncate(content, head)
}

func toolOutcome(content string) string {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") {
		return "error"
	}
	return "ok"
}

func auditDurableState(state DurableState, msgs []store.Message) DurableState {
	joined := strings.ToLower(strings.Join(append(append(append([]string{}, state.Files...), state.Verification...), state.Instructions...), "\n"))
	for i, m := range msgs {
		var parsed provider.Message
		_ = json.Unmarshal([]byte(m.ContentJSON), &parsed)
		if m.Role == "user" && !parsed.Harness && !strings.Contains(joined, strings.ToLower(truncate(parsed.Content, 80))) {
			state.Instructions = appendBounded(state.Instructions, parsed.Content, 12)
		}
		name, args := matchingCall(msgs, i, parsed.ToolCallID)
		if name == "edit" && !strings.Contains(joined, strings.ToLower(args)) {
			state.Files = appendBounded(state.Files, args, 12)
		}
		if name == "exec" && (strings.Contains(args, "test") || strings.Contains(args, "build")) && !strings.Contains(joined, strings.ToLower(args)) {
			state.Verification = appendBounded(state.Verification, args+" "+toolOutcome(parsed.Content), 12)
		}
	}
	return state
}

func recallHistory(checkpoints []store.Checkpoint, query string) []string {
	var out []string
	for _, cp := range checkpoints {
		var msgs []store.Message
		if json.Unmarshal([]byte(cp.MessagesJSON), &msgs) != nil {
			continue
		}
		for i, m := range msgs {
			if strings.Contains(strings.ToLower(m.ContentJSON), strings.ToLower(query)) {
				out = appendBounded(out, fmt.Sprintf("%s#%d: %s", cp.ID, i, truncate(m.ContentJSON, 240)), 12)
			}
		}
	}
	return out
}

func maskOldToolResults(ctx context.Context, client *jev.Client, objective string, msgs []store.Message) bool {
	changed, _ := maskOldToolResultsCached(ctx, jevRelevanceAsker(client), nil, objective, maskGate{}, msgs)
	return changed
}

func matchingCall(msgs []store.Message, idx int, id string) (string, string) {
	for i := idx - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var parsed provider.Message
		if json.Unmarshal([]byte(msgs[i].ContentJSON), &parsed) != nil {
			continue
		}
		for _, call := range parsed.ToolCalls {
			if id == "" || call.ID == id {
				args, _ := json.Marshal(call.Arguments)
				return call.Name, string(args)
			}
		}
	}
	return "tool", ""
}

// withoutRequest drops resolved entries that are the given request, matched
// loosely because summaries paraphrase ("User explicitly requested: Build
// it."). Very short requests ("ok") match only exactly, so they cannot sweep
// away unrelated entries.
func withoutRequest(resolved []string, request string) []string {
	want := normalizeRequest(request)
	if want == "" {
		return resolved
	}
	out := resolved[:0:0]
	for _, r := range resolved {
		got := normalizeRequest(r)
		match := got == want
		if !match && len(want) >= 6 {
			match = strings.Contains(got, want) || (len(got) >= 6 && strings.Contains(want, got))
		}
		if !match {
			out = append(out, r)
		}
	}
	return out
}

func normalizeRequest(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' {
			return r
		}
		if r == '\n' || r == '\t' {
			return ' '
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// compactionStateSchemaVersion is bumped when DurableState's additive fields
// change shape in a way a consumer should detect before trusting it.
const compactionStateSchemaVersion = 1

// clearedToolBytes totals the stale tool-result content already bounded to a
// head/tail pointer by capToolResult, within the window compaction is about
// to fold into a checkpoint. It is the "tool-output bytes cleared" figure
// recorded on the compaction event; clearing already-shrunk duplicate output
// is strictly safer than paraphrasing everything.
func clearedToolBytes(msgs []store.Message) int {
	total := 0
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		var msg provider.Message
		if json.Unmarshal([]byte(m.ContentJSON), &msg) != nil {
			continue
		}
		if capped := capToolResult("", msg.Content); capped != msg.Content {
			total += len(msg.Content) - len(capped)
		}
	}
	return total
}

// compactionEvidenceRefs are bounded session/event pointers a later turn can
// follow for the files this checkpoint names, instead of re-deriving them
// from prose. Each ref identifies this session and the summarized message
// range; it never embeds file contents or other evidence text.
func compactionEvidenceRefs(sid string, state DurableState, keepAt int) []string {
	refs := make([]string, 0, len(state.Files))
	for _, f := range state.Files {
		refs = appendBounded(refs, fmt.Sprintf("session:%s/messages:0-%d/%s", sid, keepAt-1, truncate(f, 200)), 24)
	}
	return refs
}
