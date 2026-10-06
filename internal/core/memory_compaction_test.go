package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func TestCompactionPersistsMemoryCandidatesAndWatermark(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(responsesText(`{"objective":"Implement feature","completed":["Feature implemented"],"verification":["go test ./... passed"],"memory_candidates":[{"kind":"command","text":"Run go test ./... for repository verification","evidence_refs":["go.mod"]}]}`))
	}))
	defer server.Close()
	cfg := config.Config{WorkspaceRoot: root, Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: server.URL}}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	m := titleModel(e.providers)
	if m.ID == "" {
		t.Fatal("no model")
	}
	const sid = "compaction-memory"
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "Implement feature", Model: m.ID, BudgetUSD: 5, WorkspacePath: root}); err != nil {
		t.Fatal(err)
	}
	event, err := st.AddEvent(ctx, sid, "tool.finished", map[string]any{"call": provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, "result": map[string]any{"ok": true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "Implemented feature and verified with go test ./..."}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Compact(ctx, sid, "manual", nil); err != nil {
		t.Fatal(err)
	}
	watermark, err := st.MemoryWatermark(ctx, sid)
	if err != nil || watermark < event.Seq {
		t.Fatalf("watermark=%d event=%d err=%v", watermark, event.Seq, err)
	}
	w, ok := e.ensureMemoryWorkspace(ctx, root)
	if !ok {
		t.Fatal("missing workspace")
	}
	records, err := st.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID})
	if err != nil || len(records) != 1 || records[0].Status != "pending" || !strings.Contains(records[0].EvidenceRefs, sid) {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}
