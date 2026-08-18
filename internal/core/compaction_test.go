package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

// This exercises the failure mode seen after a phase-boundary compaction: an
// initial direct question was answered, then a newer UI task completed. The
// next model request must receive the current wrap-up/report anchor instead of
// treating the older question as the work still to do.
func TestCompactionContinuationUsesActiveReportInsteadOfResolvedQuestion(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	workspace := t.TempDir()
	req := agentproto.TaskRequest{
		Spec:      "What time is it?",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 10_000, MaxWallClock: time.Second, MaxDepth: 0},
		Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write"},
	}
	const sid = "compaction-continuation"
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: req.Spec, Phase: "wrap-up", Status: "interrupted", BudgetUSD: req.Budget.MaxUSD, WorkspacePath: workspace, RequestJSON: store.JSON(req)}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTodos(ctx, sid, []store.Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "completed"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []provider.Message{
		{Role: "assistant", Content: "It is 14:30 UTC."},
		{Role: "user", Content: "Implement the pinned-plan UI and report the result."},
		{Role: "assistant", Content: "I updated the pinned-plan UI."},
		{Role: "assistant", Content: "The UI verification passed."},
		{Role: "assistant", Content: "The implementation is ready to wrap up."},
		{Role: "assistant", Content: "Preparing the completed-work report."},
	} {
		if err := st.AddMessage(ctx, sid, message.Role, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Compact(ctx, sid, "phase_or_context_boundary", nil); err != nil {
		t.Fatal(err)
	}
	afterCompact, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	var state DurableState
	if err := json.Unmarshal([]byte(afterCompact.DurableSummary), &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.CurrentObjective, "Wrap up the active task") || !strings.Contains(state.PendingReport, "final report") {
		t.Fatalf("missing active wrap-up anchor: %+v", state)
	}
	if len(state.ResolvedRequests) != 1 || state.ResolvedRequests[0] != req.Spec {
		t.Fatalf("resolved initial request lost: %+v", state.ResolvedRequests)
	}

	var requestChecked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for _, want := range []string{
			"CURRENT OBJECTIVE (authoritative)",
			"Wrap up the active task",
			"PENDING REPORT (authoritative)",
			"Produce the final report",
			"RESOLVED REQUESTS (do not re-answer or reopen)",
			req.Spec,
		} {
			if !strings.Contains(body.Instructions, want) {
				t.Errorf("continuation request missing %q: %s", want, body.Instructions)
			}
		}
		requestChecked.Store(true)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-terra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Completed the pinned-plan UI work and verification."}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer server.Close()
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: server.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	resultCh, err := e.Continue(ctx, sid, "Continue the active task.", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		if result.Status != agentproto.Pass || !strings.Contains(result.Result["answer"].(string), "pinned-plan UI") {
			t.Fatalf("continuation result=%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not finish")
	}
	if !requestChecked.Load() {
		t.Fatal("continuation did not reach provider")
	}
}
