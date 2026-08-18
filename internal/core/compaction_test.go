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
	if !strings.Contains(state.CurrentObjective, "Wrap up the completed") || !strings.Contains(state.CurrentObjective, "Run the pinned-plan UI verification") {
		t.Fatalf("wrap-up anchor must name the last completed todo: %+v", state)
	}
	if !strings.Contains(state.PendingReport, "final report") {
		t.Fatalf("missing active wrap-up report obligation: %+v", state)
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
			"Wrap up the completed",
			"Run the pinned-plan UI verification",
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

// An in-progress todo must win the anchor over an older answered question, and
// the first pending todo is used when nothing is in progress.
func TestCompactionAnchorSelectsActiveTodo(t *testing.T) {
	s := store.Session{Spec: "What time is it?", Phase: "implement"}
	todos := []store.Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "pending"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "in_progress"},
	}
	anchor := selectDurableAnchor(s, todos)
	if anchor.Source != "in_progress" || !strings.Contains(anchor.CurrentObjective, "Run the pinned-plan UI verification") {
		t.Fatalf("in_progress todo must win: %+v", anchor)
	}
	if anchor.TodoPosition != 1 || anchor.TodoStatus != "in_progress" {
		t.Fatalf("anchor todo metadata wrong: %+v", anchor)
	}

	noInProgress := []store.Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "pending"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "pending"},
	}
	anchor = selectDurableAnchor(s, noInProgress)
	if anchor.Source != "pending" || !strings.Contains(anchor.CurrentObjective, "Implement the pinned-plan UI") {
		t.Fatalf("first pending todo must be used: %+v", anchor)
	}
	if anchor.TodoPosition != 0 {
		t.Fatalf("first pending position wrong: %+v", anchor)
	}
}

// When every todo is complete the anchor must name the last completed work item
// rather than a generic wrap-up, so the report obligation stays concrete.
func TestCompactionAnchorAllCompleteNamesLastCompleted(t *testing.T) {
	s := store.Session{Spec: "Ship the pinned-plan UI", Phase: "wrap-up"}
	todos := []store.Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "completed"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "completed"},
	}
	anchor := selectDurableAnchor(s, todos)
	if anchor.Source != "completed_wrap_up" {
		t.Fatalf("expected completed_wrap_up source: %+v", anchor)
	}
	if !strings.Contains(anchor.CurrentObjective, "Run the pinned-plan UI verification") {
		t.Fatalf("wrap-up must name last completed todo: %+v", anchor)
	}
	if !strings.Contains(anchor.PendingReport, "Run the pinned-plan UI verification") {
		t.Fatalf("report obligation must name completed todo: %+v", anchor)
	}
	if anchor.TodoPosition != 1 {
		t.Fatalf("last completed position wrong: %+v", anchor)
	}
}

// The fallback digest must still receive the deterministic todo anchor so a
// summarizer failure cannot resurrect an older answered request.
func TestCompactionAnchorAppliesToFallback(t *testing.T) {
	s := store.Session{Spec: "What time is it?", Phase: "implement"}
	todos := []store.Todo{{Text: "Fix the retry race", Phase: "implement", Status: "in_progress"}}
	old := []store.Message{
		{Role: "user", ContentJSON: `{"content":"What time is it?"}`},
		{Role: "assistant", ContentJSON: `{"content":"It is 14:30 UTC."}`},
	}
	state := fallbackDurableState(s, old)
	state, anchor, counts := anchorDurableState(state, s, todos, old)
	if !strings.Contains(state.CurrentObjective, "Fix the retry race") {
		t.Fatalf("fallback lost active todo anchor: %+v", state)
	}
	if anchor.Source != "in_progress" {
		t.Fatalf("fallback anchor source wrong: %+v", anchor)
	}
	if counts.Detected != 1 || len(state.ResolvedRequests) != 1 {
		t.Fatalf("fallback resolved detection wrong: counts=%+v resolved=%+v", counts, state.ResolvedRequests)
	}
	if err := validateCompactionAnchor(state, anchor); err != nil {
		t.Fatalf("fallback anchor failed validation: %v", err)
	}
}

// The compacted event must carry non-sensitive anchor provenance so operators
// can see which todo drove the continuation without logging prompt content.
func TestCompactionAnchorMetadataEmitted(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	workspace := t.TempDir()
	req := agentproto.TaskRequest{
		Spec:      "Fix the retry race",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 10_000, MaxWallClock: time.Second, MaxDepth: 0},
		Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write"},
	}
	const sid = "compaction-metadata"
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: req.Spec, Phase: "implement", Status: "interrupted", BudgetUSD: req.Budget.MaxUSD, WorkspacePath: workspace, RequestJSON: store.JSON(req)}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTodos(ctx, sid, []store.Todo{
		{Text: "Fix the retry race", Phase: "implement", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []provider.Message{
		{Role: "user", Content: "Fix the retry race."},
		{Role: "assistant", Content: "I started on the retry race."},
		{Role: "assistant", Content: "The fix is in retry.go."},
		{Role: "assistant", Content: "Running the tests."},
		{Role: "assistant", Content: "Tests pass."},
		{Role: "assistant", Content: "Preparing the report."},
	} {
		if err := st.AddMessage(ctx, sid, message.Role, message); err != nil {
			t.Fatal(err)
		}
	}
	var compacted map[string]any
	emit := func(ev agentproto.AgentEvent) {
		if ev.Type == "context.compacted" {
			compacted = ev.Data.(map[string]any)
		}
	}
	if err := e.Compact(ctx, sid, "phase_or_context_boundary", emit); err != nil {
		t.Fatal(err)
	}
	if compacted == nil {
		t.Fatal("context.compacted event not emitted")
	}
	if compacted["anchor_source"] != "in_progress" {
		t.Fatalf("anchor_source wrong: %+v", compacted)
	}
	if compacted["anchor_todo_position"] != 0 || compacted["anchor_todo_status"] != "in_progress" {
		t.Fatalf("anchor todo metadata wrong: %+v", compacted)
	}
	if h, _ := compacted["anchor_hash"].(string); len(h) != 16 {
		t.Fatalf("anchor_hash must be a 16-char digest: %+v", compacted)
	}
	for _, key := range []string{"resolved_requests_prior", "resolved_requests_semantic", "resolved_requests_detected", "resolved_requests_final"} {
		if _, ok := compacted[key]; !ok {
			t.Fatalf("compacted event missing %q: %+v", key, compacted)
		}
	}
}

// Naming a completed todo as the wrap-up report subject is not reopening it,
// even when that same request was previously resolved.
func TestCompactionAnchorWrapUpAllowsResolvedCompletedTodo(t *testing.T) {
	s := store.Session{Spec: "Run the pinned-plan UI verification", Phase: "wrap-up"}
	todos := []store.Todo{{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "completed"}}
	state := DurableState{Objective: s.Spec, ResolvedRequests: []string{"Run the pinned-plan UI verification"}}
	state, anchor, _ := anchorDurableState(state, s, todos, nil)
	if err := validateCompactionAnchor(state, anchor); err != nil {
		t.Fatalf("wrap-up naming a resolved completed todo must not fail: %v", err)
	}
}

// An active todo that is itself a resolved request is a genuine conflict: the
// continuation prompt would present the same text as both current work and
// already answered. This must fail regardless of request length.
func TestCompactionAnchorRejectsResolvedActiveTodo(t *testing.T) {
	s := store.Session{Spec: "What time is it?", Phase: "implement"}
	todos := []store.Todo{{Text: "What time is it?", Phase: "implement", Status: "pending"}}
	state := DurableState{Objective: s.Spec, ResolvedRequests: []string{"What time is it?"}}
	state, anchor, _ := anchorDurableState(state, s, todos, nil)
	if err := validateCompactionAnchor(state, anchor); err == nil {
		t.Fatal("active todo equal to a resolved request must fail validation")
	}
}
