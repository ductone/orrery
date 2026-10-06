package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/jev"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

const memoryExtractionPrompt = `Extract zero to a few durable workspace memory proposals. Return JSON {"memory_candidates":[{"kind":"fact|command|decision|preference|lesson","text":"...","evidence_refs":["session/event or repository path"]}]}. Only propose evidence-backed facts true of the repository or the person beyond the current task, never session progress, speculative claims, secrets or copied tool output. Use supplied evidence pointers, not copied evidence content. Existing records and user corrections take precedence. Text must fit max_record_bytes. A question is not an instruction or a preference.`

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
	for _, candidate := range candidates {
		candidate.Text = strings.TrimSpace(candidate.Text)
		if !slices.Contains(store.MemoryKinds, candidate.Kind) || candidate.Text == "" || len(candidate.Text) > cfg.Memory.RecordBytes() {
			continue
		}
		candidate.EvidenceRefs = append(candidate.EvidenceRefs, refs...)
		candidate.EvidenceRefs = append(candidate.EvidenceRefs, "session:"+sid)
		slices.Sort(candidate.EvidenceRefs)
		candidate.EvidenceRefs = slices.Compact(candidate.EvidenceRefs)
		triaged := false
		jcfg := cfg.EffectiveJev()
		if jcfg.APIKey != "" {
			response, err := jev.New(jcfg.APIKey, jcfg.BaseURL, jcfg.Model, jcfg.Timeout()).Ask(ctx, candidate, map[string]jev.Question{"durable_memory": jev.Noul("Would this help a future session in this repository, independent of the current task?", "Evidence-backed durable repository knowledge or user preference", "Session progress, speculation or task-specific details")})
			if score := response.Answers["durable_memory"].Noul; err == nil && score != nil && *score >= 0 && *score <= 1 {
				if *response.Answers["durable_memory"].Noul < 0.5 {
					continue
				}
				triaged = true
			}
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
		operation, eventType := "commit", "memory.committed"
		if existing != nil {
			rec = *existing
			var oldRefs []string
			_ = json.Unmarshal([]byte(rec.EvidenceRefs), &oldRefs)
			candidate.EvidenceRefs = append(candidate.EvidenceRefs, oldRefs...)
			slices.Sort(candidate.EvidenceRefs)
			candidate.EvidenceRefs = slices.Compact(candidate.EvidenceRefs)
			operation, eventType = "update", "memory.updated"
		} else {
			rec = store.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: candidate.Kind, Text: candidate.Text, Provenance: "extracted", Confidence: 0.7, Status: "pending"}
			if cfg.Memory.AutoCommit && triaged {
				rec.Status = "active"
			}
			if days := cfg.Memory.ExpiresAfterDays(); days > 0 {
				expiry := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
				rec.ExpiresAt = &expiry
			}
		}
		rec.EvidenceRefs = store.JSON(candidate.EvidenceRefs)
		rec, err = e.store.ApplyMemoryMutationEvent(ctx, sid, "", eventType, map[string]any{"status": rec.Status, "provenance": "extracted"}, store.MemoryMutation{Operation: operation, Record: rec})
		if err != nil {
			return err
		}
		sessions, err := e.store.ObserveMemory(ctx, rec.ID, sid)
		if err != nil {
			return err
		}
		if rec.Status == "pending" && sessions >= 2 && triaged {
			rec.Status = "active"
			if _, err = e.store.ApplyMemoryMutationEvent(ctx, sid, "", "memory.updated", map[string]any{"id": rec.ID, "status": "active", "reason": "repeated_across_sessions"}, store.MemoryMutation{Operation: "update", Record: rec}); err != nil {
				return err
			}
		}
		e.invalidateMemoryWorkspace(w.ID)
		if rec.Status == "pending" && existing == nil {
			e.emit(ctx, sid, "memory.proposed", map[string]any{"id": rec.ID, "notice": "New memory proposal; inspect and confirm with the memory tool on the person's instruction."}, nil)
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
				if finished.Call.Name == "edit" || finished.Call.Name == "exec" && verificationKind(command) == fullCheck {
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
		case "session.terminal", "verification.accepted", "verification.judged":
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
