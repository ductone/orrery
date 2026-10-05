package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestDeclines(t *testing.T) {
	for answer, want := range map[string]bool{
		"Stop here": true, "stop": true, "No.": true, "no, that's enough": true, "cancel": true,
		"Keep going": false, "Add $50.00 and continue": false, "yes": false,
		"keep going but focus on the tests": false, "notice the failing test first": false, "": false,
	} {
		if got := declines(answer); got != want {
			t.Errorf("declines(%q) = %v, want %v", answer, got, want)
		}
	}
}

func waitResult(t *testing.T, ch <-chan agentproto.TaskResult) agentproto.TaskResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	return agentproto.TaskResult{}
}

// A session that reaches its dollar budget pauses with a question instead of
// ending; agreeing extends the budget and the work continues.
func TestBudgetPausesAndResumesOnAgreement(t *testing.T) {
	e, st := testEngine(t)
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 1, JobDefaultFraction: .2}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "task", Phase: "plan", BudgetUSD: 1, SpentUSD: 1.5, WorkspacePath: workspace}); err != nil {
		t.Fatal(err)
	}
	req := agentproto.TaskRequest{Spec: "task", Budget: agentproto.Budget{MaxUSD: 1, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	first := e.run(ctx, sid, "", req, nil)
	input, _ := first.Result["input"].(agentproto.InputRequest)
	if first.Status != agentproto.InputRequired || !strings.HasPrefix(input.ID, budgetQuestion) || !strings.Contains(input.Question, "$1.50 of this session's $1.00") || input.Choices[0] != budgetChoice(1) {
		t.Fatalf("first = %+v", first)
	}
	results, err := e.Continue(ctx, sid, input.Choices[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	second := waitResult(t, results)
	if second.Status != agentproto.Pass {
		t.Fatalf("after agreeing, the work continues: %+v", second)
	}
	if after, _ := st.Session(ctx, sid); after.BudgetUSD != 2 {
		t.Fatalf("budget = %v, want one more session's worth", after.BudgetUSD)
	}
}

func TestDecliningAHarnessQuestionStops(t *testing.T) {
	e, st := testEngine(t)
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "task", Phase: "plan", BudgetUSD: 1, SpentUSD: 2, WorkspacePath: workspace})
	req := agentproto.TaskRequest{Spec: "task", Budget: agentproto.Budget{MaxUSD: 1, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	if r := e.run(ctx, sid, "", req, nil); r.Status != agentproto.InputRequired {
		t.Fatalf("r = %+v", r)
	}
	results, err := e.Continue(ctx, sid, choiceStop, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := waitResult(t, results); r.Status != agentproto.Cancelled || !strings.Contains(r.Error, "person's request") {
		t.Fatalf("stopping ends the session cleanly: %+v", r)
	}
	if after, _ := st.Session(ctx, sid); after.BudgetUSD != 1 {
		t.Fatal("declining must not extend the budget")
	}
	if len(s.requests) != 0 {
		t.Fatal("no model call after the person said stop")
	}
}

// A model that keeps returning empty replies is set aside and another model
// finishes the work, instead of the run failing.
func TestMisbehavingModelIsSetAside(t *testing.T) {
	e, _ := testEngine(t)
	var models []string
	s := &scriptedResponses{reply: func(_ int, body map[string]any) map[string]any {
		m := body["model"].(string)
		models = append(models, m)
		if m == models[0] {
			return map[string]any{"model": m, "status": "completed", "output": []any{map[string]any{"type": "reasoning", "summary": []any{}}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 50}}
		}
		return responsesText("done")
	}}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	cfg.Router = config.RouterConfig{LambdaCost: .35}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "answer", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	_, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, results)
	if r.Status != agentproto.Pass || models[len(models)-1] == models[0] {
		t.Fatalf("status=%s models=%v", r.Status, models)
	}
}

func TestWorkerOutOfBudgetReturnsWhatItFound(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "explore", BudgetUSD: 1, SpentUSD: 2, WorkspacePath: t.TempDir()})
	_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "The handler lives in internal/web/server.go."})
	r := e.run(ctx, sid, "job-1", agentproto.TaskRequest{Spec: "explore", Budget: agentproto.Budget{MaxUSD: 1, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "read"}}, nil)
	if r.Status != agentproto.BudgetExhausted || r.Result["partial"] != true || !strings.Contains(r.Result["findings"].(string), "server.go") {
		t.Fatalf("a worker's budget is still a hard slice, but it returns its findings: %+v", r)
	}
}
