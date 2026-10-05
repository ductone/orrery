package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestCapToolResult(t *testing.T) {
	small := `{"ok":true}`
	if capToolResult("read", small) != small {
		t.Fatal("small results are untouched")
	}
	big := `{"content":"` + strings.Repeat("a", 500_000) + `END"}`
	capped := capToolResult("fetch", big)
	if len(capped) > maxToolResultChars+2_000 {
		t.Fatalf("capped to %d chars", len(capped))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(capped), &out); err != nil {
		t.Fatalf("a capped result must stay valid JSON: %v", err)
	}
	if out["truncated"] != true || out["tool"] != "fetch" || int(out["original_chars"].(float64)) != len(big) || !strings.HasSuffix(out["tail"].(string), `END"}`) {
		t.Fatalf("capped = %v", out["hint"])
	}
}

func TestShrinkStoredToolMessages(t *testing.T) {
	_, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "x", BudgetUSD: 1})
	_ = st.AddMessage(ctx, sid, "tool", provider.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("h", 2_700_000)})
	_ = st.AddMessage(ctx, sid, "tool", provider.Message{Role: "tool", ToolCallID: "c2", Content: "small"})
	_ = st.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: strings.Repeat("u", 200_000)})
	n, err := st.ShrinkMessages(ctx, sid, "tool", maxToolResultChars, shrinkStoredToolMessage)
	if err != nil || n != 1 {
		t.Fatalf("shrunk=%d err=%v", n, err)
	}
	msgs, _ := st.Messages(ctx, sid)
	var first provider.Message
	_ = json.Unmarshal([]byte(msgs[0].ContentJSON), &first)
	if first.ToolCallID != "c1" || len(first.Content) > maxToolResultChars+2_000 || !strings.Contains(first.Content, "truncated") {
		t.Fatalf("first tool message: id=%s len=%d", first.ToolCallID, len(first.Content))
	}
	if len(msgs[2].ContentJSON) < 200_000 {
		t.Fatal("only the named role is shrunk")
	}
}

// A session whose stored tool results exceed every context window (as raw
// HTML fetches once did) recovers instead of failing with "no compatible
// models".
func TestOversizedHistoryRecovers(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	e, st := testEngine(t)
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "research memory", Phase: "explore", BudgetUSD: 5, WorkspacePath: workspace})
	_ = st.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: "research memory"})
	for i := range 3 {
		id := "f" + string(rune('0'+i))
		_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: id, Name: "fetch", Arguments: map[string]any{"url": "https://example.com"}}}})
		_ = st.AddMessage(ctx, sid, "tool", provider.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("<div>", 600_000)})
	}
	result := e.run(ctx, sid, "", agentproto.TaskRequest{Spec: "research memory", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 10_000_000, MaxWallClock: 60e9}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}, nil)
	if result.Status != agentproto.Pass {
		t.Fatalf("result = %+v", result)
	}
	var recovered bool
	es, _ := st.EventsAfter(ctx, sid, 0)
	for _, ev := range es {
		recovered = recovered || ev.Type == "context.oversized" && strings.Contains(string(ev.Data), `"tool_results_shrunk":3`)
	}
	if !recovered {
		t.Fatal("the oversized history must be recorded and shrunk")
	}
}

func TestWebSearchIsOnlyOfferedWhenConfigured(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	result := runScripted(t, s, "shared-write")
	if result.Status != agentproto.Pass {
		t.Fatalf("result = %+v", result)
	}
	for _, raw := range s.requests[0]["tools"].([]any) {
		if raw.(map[string]any)["name"] == "web_search" {
			t.Fatal("web_search must not be offered without a search key")
		}
	}
}

func gateConfig(workspace, url string) config.Config {
	return config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: url}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
}
