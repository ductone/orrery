package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

const memoryExtractionPrompt = `Extract zero to a few durable workspace memory proposals. Return JSON {"memory_candidates":[{"kind":"fact|command|decision|preference|lesson","text":"...","evidence_refs":["session/event or repository path"]}]}. Only propose evidence-backed facts true of the repository or the person beyond the current task, never session progress, speculative claims, secrets or copied tool output. Use supplied evidence pointers, not copied evidence content. Existing records and user corrections take precedence. Text must fit max_record_bytes. A question is not an instruction or a preference.`

const (
	// memoryActivateScore and memoryDiscardScore bound the triage: at or above
	// the first a candidate is active immediately, below the second it is
	// dropped, and in between it stays pending (and is not shown to the model).
	memoryActivateScore = 0.75
	memoryDiscardScore  = 0.5
)

const memoryTriageQuestion = "Does this record state knowledge a future session could not learn by reading the code: a convention, gotcha, workflow, command or preference? Reject descriptions of what code does or how it is structured."

// triageMemory scores a candidate with Jev. It reports false when no valid
// score is available, in which case the candidate is not recorded.
func (e *Engine) triageMemory(ctx context.Context, jcfg config.JevConfig, candidate MemoryCandidate) (float64, bool) {
	if jcfg.APIKey == "" {
		return 0, false
	}
	q := jev.Noul(memoryTriageQuestion, "A convention, gotcha, workflow, command or preference not evident from the code", "A description of code behaviour or structure, session progress, speculation or task-specific detail")
	response, err := jev.New(jcfg.APIKey, jcfg.BaseURL, jcfg.Model, jcfg.Timeout()).Ask(ctx, candidate, map[string]jev.Question{"durable_memory": q})
	if err != nil {
		return 0, false
	}
	score := response.Answers["durable_memory"].Noul
	if score == nil || *score < 0 || *score > 1 {
		return 0, false
	}
	return *score, true
}

// conflictingMemory returns an active record of the same kind that the
// candidate contradicts, or nil.
func (e *Engine) conflictingMemory(ctx context.Context, jcfg config.JevConfig, candidate MemoryCandidate, records []store.MemoryRecord) *store.MemoryRecord {
	if jcfg.APIKey == "" {
		return nil
	}
	questions := map[string]jev.Question{}
	cands := map[string]*store.MemoryRecord{}
	for i := range records {
		if records[i].Status != "active" || records[i].Kind != candidate.Kind || len(cands) >= 5 {
			continue
		}
		key := "conflict_" + records[i].ID
		cands[key] = &records[i]
		questions[key] = jev.Noul("Does the new candidate contradict this existing memory, so that both cannot be true? Existing memory: "+records[i].Text, "Contradicts", "Compatible or unrelated")
	}
	if len(questions) == 0 {
		return nil
	}
	response, err := jev.New(jcfg.APIKey, jcfg.BaseURL, jcfg.Model, jcfg.Timeout()).Ask(ctx, candidate, questions)
	if err != nil {
		return nil
	}
	for key, rec := range cands {
		if s := response.Answers[key].Noul; s != nil && *s >= memoryActivateScore {
			return rec
		}
	}
	return nil
}

// memoryTempDir is the OS temp dir; tests override it because their
// fixtures also live there.
var memoryTempDir = os.TempDir

// memoryTempWorkspace reports whether path is under the OS temp dir, as
// benchmark/eval fixtures are.
func memoryTempWorkspace(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	tmp := memoryTempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	rel, err := filepath.Rel(tmp, abs)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func memoryDuplicate(a, b string) bool {
	aTokens, bTokens := strings.Fields(strings.ToLower(a)), strings.Fields(strings.ToLower(b))
	if strings.Join(aTokens, " ") == strings.Join(bTokens, " ") {
		return true
	}
	if len(aTokens) < 5 || len(bTokens) < 5 {
		return false
	}
	set := map[string]bool{}
	for _, token := range aTokens {
		set[strings.Trim(token, ".,:;!?()")] = true
	}
	other := map[string]bool{}
	for _, token := range bTokens {
		other[strings.Trim(token, ".,:;!?()")] = true
	}
	common := 0
	for token := range set {
		if other[token] {
			common++
		}
	}
	return float64(common)/float64(max(len(set), len(other))) >= 0.9
}

func (e *Engine) persistMemoryCandidates(ctx context.Context, sid, path string, candidates []MemoryCandidate, refs []string) error {
	e.memoryExtractionMu.Lock()
	defer e.memoryExtractionMu.Unlock()
	cfg, _, _, _, _ := e.runtimeSnapshot()
	w, ok := e.ensureMemoryWorkspace(ctx, path)
	if !ok {
		return fmt.Errorf("memory workspace unavailable")
	}
	if memoryTempWorkspace(path) {
		return nil
	}
	e.retriagePendingMemory(ctx, sid, w, cfg.EffectiveJev(), false)
	for _, candidate := range candidates {
		candidate.Text = strings.TrimSpace(candidate.Text)
		if !slices.Contains(store.MemoryKinds, candidate.Kind) || candidate.Text == "" || len(candidate.Text) > cfg.Memory.RecordBytes() {
			continue
		}
		candidate.EvidenceRefs = append(candidate.EvidenceRefs, refs...)
		candidate.EvidenceRefs = append(candidate.EvidenceRefs, "session:"+sid)
		slices.Sort(candidate.EvidenceRefs)
		candidate.EvidenceRefs = slices.Compact(candidate.EvidenceRefs)
		jcfg := cfg.EffectiveJev()
		// Without a usable triage score the candidate stays pending (never
		// injected) until retriagePendingMemory can score it.
		score, triaged := e.triageMemory(ctx, jcfg, candidate)
		confidence := 0.7
		if triaged {
			confidence = score
		}
		if triaged && score < memoryDiscardScore {
			continue
		}
		records, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true, IncludeDeleted: true})
		if err != nil {
			return err
		}
		var existing *store.MemoryRecord
		blocked := false
		for i := range records {
			if !memoryDuplicate(records[i].Text, candidate.Text) {
				continue
			}
			if records[i].Status == "deleted" || records[i].Status == "superseded" || records[i].Status == "expired" {
				blocked = true
				break
			}
			if existing == nil {
				existing = &records[i]
			}
		}
		if blocked {
			continue
		}
		var rec store.MemoryRecord
		operation, eventType, oldID := "commit", "memory.committed", ""
		if existing != nil {
			rec = *existing
			var oldRefs []string
			_ = json.Unmarshal([]byte(rec.EvidenceRefs), &oldRefs)
			candidate.EvidenceRefs = append(candidate.EvidenceRefs, oldRefs...)
			slices.Sort(candidate.EvidenceRefs)
			candidate.EvidenceRefs = slices.Compact(candidate.EvidenceRefs)
			operation, eventType = "update", "memory.updated"
		} else {
			rec = store.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: candidate.Kind, Text: candidate.Text, Provenance: "extracted", Confidence: confidence, Status: "pending"}
			if triaged && score >= memoryActivateScore {
				rec.Status = "active"
				if old := e.conflictingMemory(ctx, jcfg, candidate, records); old != nil {
					oldID, operation, eventType = old.ID, "correct", "memory.updated"
				}
			}
			if days := cfg.Memory.ExpiresAfterDays(); days > 0 {
				expiry := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
				rec.ExpiresAt = &expiry
			}
		}
		rec.EvidenceRefs = store.JSON(candidate.EvidenceRefs)
		rec, err = e.store.ApplyMemoryMutationEvent(ctx, sid, "", eventType, map[string]any{"status": rec.Status, "provenance": "extracted", "triage_score": confidence}, store.MemoryMutation{Operation: operation, WorkspaceID: w.ID, OldID: oldID, Record: rec})
		if err != nil {
			return err
		}
		if _, err = e.store.ObserveMemory(ctx, rec.ID, sid); err != nil {
			return err
		}
		e.invalidateMemoryWorkspace(w.ID)
		if oldID != "" {
			e.emit(ctx, sid, "memory.contradicted", map[string]any{"id": oldID, "replaced_by": rec.ID}, nil)
		}
	}
	return nil
}

func (e *Engine) scheduleMemoryExtraction(ctx context.Context, sid string) {
	e.shadowWG.Add(1)
	go func() {
		defer e.shadowWG.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := e.extractMemory(ctx, sid); err != nil {
			e.emit(ctx, sid, "memory.extraction_failed", map[string]any{"error": err.Error()}, nil)
		}
	}()
}

func (e *Engine) catchUpMemory(ctx context.Context) {
	e.shadowWG.Add(1)
	go func() {
		defer e.shadowWG.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		e.cleanupPendingMemory(ctx)
	}()
	sessions, err := e.store.Sessions(ctx)
	if err != nil {
		return
	}
	for _, session := range sessions {
		if session.Status != "running" && session.ParentSessionID == "" {
			e.scheduleMemoryExtraction(ctx, session.ID)
		}
	}
}

func (e *Engine) extractMemory(ctx context.Context, sid string) error {
	e.memoryRunMu.Lock()
	defer e.memoryRunMu.Unlock()
	watermark, err := e.store.MemoryWatermark(ctx, sid)
	if err != nil {
		return err
	}
	events, err := e.store.EventsAfter(ctx, sid, watermark)
	if err != nil {
		return err
	}
	end := -1
	for i, event := range events {
		if event.Type == "session.terminal" {
			var terminal struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(event.Data, &terminal)
			if terminal.Status == "pass" {
				end = i
			}
		}
	}
	if end < 0 {
		return nil
	}
	events = events[:end+1]
	useful := false
	refs := []string{}
	inputEvents := []store.Event{}
	for _, event := range events {
		switch event.Type {
		case "tool.finished":
			var finished struct {
				Call   provider.ToolCall `json:"call"`
				Result map[string]any    `json:"result"`
			}
			if json.Unmarshal(event.Data, &finished) == nil && finished.Result["error"] == nil && finished.Result["ok"] != false {
				command, _ := finished.Call.Arguments["command"].(string)
				if finished.Call.Name == "edit" || finished.Call.Name == "exec" && command != "" {
					useful = true
				}
				inputEvents = append(inputEvents, event)
			}
		case "session.started", "user.message":
			var message struct {
				Spec    string `json:"spec"`
				Content string `json:"content"`
			}
			if json.Unmarshal(event.Data, &message) == nil && memoryInstruction(message.Spec+message.Content) {
				useful = true
			}
			inputEvents = append(inputEvents, event)
		case "memory.extraction_input":
			var input struct {
				Files  []string        `json:"files_changed"`
				Checks []commandRecord `json:"checks"`
			}
			if json.Unmarshal(event.Data, &input) == nil && (len(input.Files) > 0 || len(input.Checks) > 0) {
				useful = true
			}
			inputEvents = append(inputEvents, event)
		case "session.terminal", "verification.accepted", "verification.advised":
			inputEvents = append(inputEvents, event)
		}
		refs = append(refs, event.EventID)
	}
	if !useful {
		return e.store.AdvanceMemoryWatermark(ctx, sid, events[len(events)-1].Seq)
	}
	session, err := e.store.Session(ctx, sid)
	if err != nil {
		return err
	}
	cfg, registry, _, _, _ := e.runtimeSnapshot()
	if strings.TrimSpace(session.WorkspacePath) == "" {
		session.WorkspacePath = cfg.WorkspaceRoot
	}
	if memoryTempWorkspace(session.WorkspacePath) {
		return e.store.AdvanceMemoryWatermark(ctx, sid, events[len(events)-1].Seq)
	}
	if registry == nil {
		return fmt.Errorf("memory extraction provider unavailable")
	}
	w, ok := e.ensureMemoryWorkspace(ctx, session.WorkspacePath)
	if !ok {
		return fmt.Errorf("memory workspace unavailable")
	}
	records, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil {
		return err
	}
	m := titleModel(registry)
	if m.ID == "" {
		return fmt.Errorf("memory extraction model unavailable")
	}
	input := store.JSON(map[string]any{"durable_state": session.DurableSummary, "events": inputEvents, "existing_records": records, "max_record_bytes": cfg.Memory.RecordBytes()})
	response, err := registry.CompleteOne(ctx, router.Decision{Model: m, Effort: model.EffortLow}, func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
		return provider.Request{System: memoryExtractionPrompt, Messages: []provider.Message{{Role: "user", Content: input}}, MaxOutput: min(2000, m.MaxOutput), Effort: d.Effort}, nil
	})
	if err != nil {
		return err
	}
	var output struct {
		Candidates []MemoryCandidate `json:"memory_candidates"`
	}
	if err := json.Unmarshal([]byte(response.Message.Content), &output); err != nil {
		return err
	}
	if err := e.persistMemoryCandidates(ctx, sid, session.WorkspacePath, output.Candidates, refs); err != nil {
		return err
	}
	return e.store.AdvanceMemoryWatermark(ctx, sid, events[len(events)-1].Seq)
}

func memoryInstruction(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" || strings.Contains(text, "?") {
		return false
	}
	for _, prefix := range []string{"what ", "which ", "why ", "where ", "when ", "who ", "how ", "is ", "are ", "does ", "do you ", "can you explain "} {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return true
}

// memoryTriageConcurrency bounds parallel Jev calls when re-triaging records.
var memoryTriageConcurrency = 16

// triageMany scores every record concurrently, bounded by
// memoryTriageConcurrency. oks[i] is false when record i got no valid score.
func (e *Engine) triageMany(ctx context.Context, jcfg config.JevConfig, recs []store.MemoryRecord) ([]float64, []bool) {
	scores, oks := make([]float64, len(recs)), make([]bool, len(recs))
	sem := make(chan struct{}, max(1, memoryTriageConcurrency))
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			scores[i], oks[i] = e.triageMemory(ctx, jcfg, MemoryCandidate{Kind: rec.Kind, Text: rec.Text})
		}()
	}
	wg.Wait()
	return scores, oks
}

// retriagePendingMemory applies the current triage to pending records in a
// workspace: records at or above the activation score become active, those
// below the discard score are forgotten, and the rest stay pending. It lets
// records created before automatic triage, or while Jev was unavailable, be
// resolved without asking the person.
func (e *Engine) retriagePendingMemory(ctx context.Context, sid string, w store.Workspace, jcfg config.JevConfig, discardUncertain bool) {
	pending, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, Status: "pending"})
	if err != nil {
		return
	}
	changed := false
	scores, oks := e.triageMany(ctx, jcfg, pending)
	for i, rec := range pending {
		score := scores[i]
		if !oks[i] {
			continue
		}
		switch {
		case score >= memoryActivateScore && sid == "":
			rec.Status, rec.Confidence = "active", score
			err = e.store.UpdateMemory(ctx, rec)
		case (score < memoryDiscardScore || discardUncertain) && sid == "":
			err = e.store.ForgetMemory(ctx, w.ID, rec.ID)
		case score >= memoryActivateScore:
			rec.Status, rec.Confidence = "active", score
			_, err = e.store.ApplyMemoryMutationEvent(ctx, sid, "", "memory.updated", map[string]any{"id": rec.ID, "status": "active", "reason": "retriaged", "triage_score": score}, store.MemoryMutation{Operation: "update", Record: rec})
		case score < memoryDiscardScore:
			_, err = e.store.ApplyMemoryMutationEvent(ctx, sid, "", "memory.forgotten", map[string]any{"id": rec.ID, "to_status": "deleted", "reason": "retriaged", "triage_score": score}, store.MemoryMutation{Operation: "forget", WorkspaceID: w.ID, MemoryID: rec.ID})
		default:
			continue
		}
		if err == nil {
			changed = true
		}
	}
	if changed {
		e.invalidateMemoryWorkspace(w.ID)
	}
}

// cleanupPendingMemory is the startup sweep of pending records left by the
// confirmation-based flow: pending records in temporary (benchmark/eval)
// workspaces are deleted, and elsewhere pending records are re-triaged, the
// passing ones activated and the rest deleted. It does nothing without Jev,
// so untriaged records are never discarded blindly outside temp workspaces.
func (e *Engine) cleanupPendingMemory(ctx context.Context) {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	workspaces, err := e.store.MemoryWorkspaces(ctx)
	if err != nil {
		return
	}
	jcfg := cfg.EffectiveJev()
	for _, w := range workspaces {
		if path, ok := strings.CutPrefix(w.IdentityKey, "path:"); ok && memoryTempWorkspace(path) {
			pending, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, Status: "pending"})
			if err != nil {
				continue
			}
			for _, rec := range pending {
				_ = e.store.ForgetMemory(ctx, w.ID, rec.ID)
			}
			if len(pending) > 0 {
				e.invalidateMemoryWorkspace(w.ID)
			}
			continue
		}
		e.memoryExtractionMu.Lock()
		e.retriagePendingMemory(ctx, "", w, jcfg, true)
		e.memoryExtractionMu.Unlock()
	}
}
