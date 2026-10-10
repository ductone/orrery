package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func TestMemoryExtractionQuestionSkipsProvider(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	e.providers = nil
	if _, err := e.store.AddEvent(ctx, "s1", "session.started", map[string]any{"spec": "What does this package do?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddEvent(ctx, "s1", "session.terminal", map[string]any{"status": "pass", "result": "It handles requests."}); err != nil {
		t.Fatal(err)
	}
	if err := e.extractMemory(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	watermark, err := e.store.MemoryWatermark(ctx, "s1")
	if err != nil || watermark == 0 {
		t.Fatalf("watermark = %d, %v", watermark, err)
	}
	if err := e.extractMemory(ctx, "s1"); err != nil {
		t.Fatal(err)
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
	e.catchUpMemory(ctx)
	e.waitBackground()
	if calls.Load() != 1 {
		t.Fatalf("extraction repeated: %d calls", calls.Load())
	}
}
