package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/classify"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// memoryGateEngine is a memory engine whose extraction model is a test server
// that counts calls and proposes one record. A nil gate leaves Jev unset.
func memoryGateEngine(t *testing.T, gate func(name string) (classify.Answer, error)) (*Engine, *atomic.Int32) {
	t.Helper()
	e := memoryEngine(t, config.MemoryConfig{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(responsesText(`{"memory_candidates":[{"kind":"fact","text":"Gowthami and Erik triage IGA issues, not Justin","evidence_refs":[]}]}`))
	}))
	t.Cleanup(srv.Close)
	e.cfg.Providers = map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}
	e.cfg.WorkspaceRoot = t.TempDir()
	e.providers = provider.New(e.cfg)
	if gate != nil {
		e.UseClassifier(&classify.Fake{Answer: func(_ any, name string, _ classify.Question) (classify.Answer, error) { return gate(name) }})
	}
	return e, &calls
}

// conversationSession records a passed session with a question in its thread
// history, a read-only tool call and no edits.
func conversationSession(t *testing.T, e *Engine) store.Event {
	t.Helper()
	ctx := context.Background()
	spec := "Thread so far: who triages IGA? Justin.\nLatest message: the main people I'd expect to be doing triage for IGA are Gowthami and Erik. Definitely not Justin."
	if _, err := e.store.AddEvent(ctx, "s1", "session.started", map[string]any{"spec": spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "tool.finished", map[string]any{"call": provider.ToolCall{Name: "linear_search", Arguments: map[string]any{"query": "IGA"}}, "result": map[string]any{"ok": true, "summary": "3 issues"}}); err != nil {
		t.Fatal(err)
	}
	terminal, err := e.store.AddEvent(ctx, "s1", "session.terminal", map[string]any{"status": "pass", "result": "Thanks, noted."})
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func gateEvents(t *testing.T, e *Engine) []map[string]any {
	t.Helper()
	events, err := e.store.EventsAfter(context.Background(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Type == "memory.extraction_gate" {
			var data map[string]any
			_ = json.Unmarshal(ev.Data, &data)
			out = append(out, data)
		}
	}
	return out
}

func memoryRecords(t *testing.T, e *Engine) []store.MemoryRecord {
	t.Helper()
	w, ok := e.ensureMemoryWorkspace(context.Background(), e.cfg.WorkspaceRoot)
	if !ok {
		t.Fatal("workspace missing")
	}
	records, err := e.store.ListMemory(context.Background(), store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestMemoryGateJevYesExtracts(t *testing.T) {
	e, calls := memoryGateEngine(t, func(string) (classify.Answer, error) { return classify.Prob(0.9), nil })
	terminal := conversationSession(t, e)
	if err := e.extractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(memoryRecords(t, e)) != 1 {
		t.Fatalf("calls=%d records=%+v", calls.Load(), memoryRecords(t, e))
	}
	if wm, _ := e.store.MemoryWatermark(context.Background(), "s1"); wm != terminal.Seq {
		t.Fatalf("watermark=%d want %d", wm, terminal.Seq)
	}
	gates := gateEvents(t, e)
	if len(gates) != 1 || gates[0]["source"] != "jev" || gates[0]["extract"] != true || gates[0]["probability"] != 0.9 || gates[0]["question_version"] != memoryGateVersion {
		t.Fatalf("gate events = %+v", gates)
	}
}

func TestMemoryGateJevNoSkips(t *testing.T) {
	var state any
	e, calls := memoryGateEngine(t, func(string) (classify.Answer, error) { return classify.Prob(0.1), nil })
	e.UseClassifier(&classify.Fake{Answer: func(s any, _ string, _ classify.Question) (classify.Answer, error) {
		state = s
		return classify.Prob(0.1), nil
	}})
	e.providers = nil
	terminal := conversationSession(t, e)
	if err := e.extractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if wm, _ := e.store.MemoryWatermark(context.Background(), "s1"); wm != terminal.Seq {
		t.Fatalf("watermark=%d want %d", wm, terminal.Seq)
	}
	if calls.Load() != 0 {
		t.Fatalf("extraction ran %d times", calls.Load())
	}
	encoded := store.JSON(state)
	for _, text := range []string{"Definitely not Justin", "linear_search", "Thanks, noted."} {
		if !strings.Contains(encoded, text) {
			t.Errorf("gate state missing %q: %s", text, encoded)
		}
	}
	if gates := gateEvents(t, e); len(gates) != 1 || gates[0]["extract"] != false {
		t.Fatalf("gate events = %+v", gates)
	}
	// The gate's own event, after the terminal, does not make the session pending again.
	if err := e.extractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if gates := gateEvents(t, e); len(gates) != 1 {
		t.Fatalf("gate asked again: %+v", gates)
	}
}

func TestMemoryGateFailsOpen(t *testing.T) {
	for name, gate := range map[string]func(string) (classify.Answer, error){
		"unavailable": nil,
		"error":       func(string) (classify.Answer, error) { return classify.Answer{}, errors.New("jev down") },
	} {
		t.Run(name, func(t *testing.T) {
			e, calls := memoryGateEngine(t, gate)
			conversationSession(t, e)
			if err := e.extractMemory(context.Background(), "s1"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(memoryRecords(t, e)) != 1 {
				t.Fatalf("calls=%d records=%+v", calls.Load(), memoryRecords(t, e))
			}
			if gates := gateEvents(t, e); len(gates) != 1 || gates[0]["source"] != name || gates[0]["extract"] != true {
				t.Fatalf("gate events = %+v", gates)
			}
		})
	}
}

func TestMemoryGateSkippedForEdits(t *testing.T) {
	asked := false
	e, calls := memoryGateEngine(t, func(name string) (classify.Answer, error) {
		if name == "worth_remembering" {
			asked = true
		}
		return classify.Prob(0.1), nil
	})
	ctx := context.Background()
	if _, err := e.store.AddEvent(ctx, "s1", "session.started", map[string]any{"spec": "What does this do?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "tool.finished", map[string]any{"call": provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, "result": map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "session.terminal", map[string]any{"status": "pass", "result": "Done."}); err != nil {
		t.Fatal(err)
	}
	if err := e.extractMemory(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if asked || calls.Load() != 1 || len(gateEvents(t, e)) != 0 {
		t.Fatalf("asked=%v calls=%d gates=%+v", asked, calls.Load(), gateEvents(t, e))
	}
}

func TestReextractMemoryIgnoresWatermarkAndGate(t *testing.T) {
	e, calls := memoryGateEngine(t, func(name string) (classify.Answer, error) {
		if name == "worth_remembering" {
			return classify.Prob(0.1), nil
		}
		return classify.Prob(0.9), nil
	})
	terminal := conversationSession(t, e)
	if err := e.extractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("gate did not skip: %d calls", calls.Load())
	}
	if err := e.ReextractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	// Records are not duplicated by a second request.
	if err := e.ReextractMemory(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(memoryRecords(t, e)) != 1 {
		t.Fatalf("calls=%d records=%+v", calls.Load(), memoryRecords(t, e))
	}
	if wm, _ := e.store.MemoryWatermark(context.Background(), "s1"); wm != terminal.Seq {
		t.Fatalf("watermark=%d want %d", wm, terminal.Seq)
	}
	if err := e.ReextractMemory(context.Background(), "missing"); err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

func TestMemoryExtractionFailureRetainsWatermark(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	e.providers = nil
	if _, err := e.store.AddEvent(ctx, "s1", "tool.finished", map[string]any{"call": provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, "result": map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "session.terminal", map[string]any{"status": "pass", "result": "Implemented."}); err != nil {
		t.Fatal(err)
	}
	if err := e.extractMemory(ctx, "s1"); err == nil {
		t.Fatal("expected unavailable-provider failure")
	}
	watermark, err := e.store.MemoryWatermark(ctx, "s1")
	if err != nil || watermark != 0 {
		t.Fatalf("watermark = %d, %v", watermark, err)
	}
}

func TestMemoryCompletionAndCatchUp(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded := store.JSON(body)
		for _, text := range []string{"go test ./...", "feature.go", "Implemented", "durable_state"} {
			if !strings.Contains(encoded, text) {
				t.Errorf("missing %s in extraction input", text)
			}
		}
		_ = json.NewEncoder(w).Encode(responsesText(`{"memory_candidates":[{"kind":"command","text":"Run go test ./... to verify this repository","evidence_refs":["go.mod"]}]}`))
	}))
	defer srv.Close()
	e.cfg.Providers = map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}
	e.cfg.WorkspaceRoot = t.TempDir()
	e.providers = provider.New(e.cfg)
	if _, err := e.store.AddEvent(ctx, "s1", "tool.finished", map[string]any{"call": provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, "result": map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "tool.finished", map[string]any{"call": provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "go test ./..."}}, "result": map[string]any{"ok": true, "summary": "PASS"}}); err != nil {
		t.Fatal(err)
	}
	terminal, err := e.store.AddEvent(ctx, "s1", "session.terminal", map[string]any{"status": "pass", "result": "Implemented"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := e.store.Session(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	session.Status = "pass"
	if err := e.store.UpdateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	e.catchUpMemory(ctx)
	e.waitBackground()
	watermark, err := e.store.MemoryWatermark(ctx, "s1")
	if err != nil || watermark != terminal.Seq {
		t.Fatalf("watermark=%d want=%d err=%v", watermark, terminal.Seq, err)
	}
	w, ok := e.ensureMemoryWorkspace(ctx, e.cfg.WorkspaceRoot)
	if !ok {
		t.Fatal("workspace missing")
	}
	records, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil || len(records) != 1 || records[0].Status != "pending" || records[0].Provenance != "extracted" || !strings.Contains(records[0].EvidenceRefs, terminal.EventID) {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if records[0].SessionID != "s1" || records[0].Origin != workspaceIdentityKey(e.cfg.WorkspaceRoot) || records[0].Scope != storage.ScopeAgent {
		t.Fatalf("extraction provenance = %+v", records[0])
	}
	e.catchUpMemory(ctx)
	e.waitBackground()
	if calls.Load() != 1 {
		t.Fatalf("extraction repeated: %d calls", calls.Load())
	}
}
