package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/google/uuid"
)

// handlerEngine is an engine over a scripted model with the given options.
func handlerEngine(t *testing.T, reply func(n int, body map[string]any) map[string]any, opts ...Option) (*Engine, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := (&scriptedResponses{reply: reply}).serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 1, JobDefaultFraction: .2}
	return New(cfg, st, provider.New(cfg), nil, opts...), st, workspace
}

func askThenDone(n int, _ map[string]any) map[string]any {
	if n == 1 {
		return responsesCall("c1", "ask", map[string]any{"question": "Which file?"})
	}
	return responsesText("done")
}

func handlerRequest(workspace string) agentproto.TaskRequest {
	return agentproto.TaskRequest{Spec: "task", Budget: agentproto.Budget{MaxUSD: 1, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
}

// overBudget seeds a session that has spent more than its budget and starts a
// turn on it, so the first thing the run does is ask about the budget.
func overBudget(t *testing.T, e *Engine, st *store.Store, workspace string, spent float64) agentproto.TaskResult {
	t.Helper()
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "task", Phase: "plan", BudgetUSD: 1, SpentUSD: spent, WorkspacePath: workspace, RequestJSON: store.JSON(handlerRequest(workspace))}); err != nil {
		t.Fatal(err)
	}
	results, err := e.Continue(ctx, sid, "go on", nil)
	if err != nil {
		t.Fatal(err)
	}
	return waitResult(t, results)
}

// budgetPolicy is orrery.AutoAnswer's budget rule, which this package cannot
// import: extend until the budget would exceed max, then stop.
func budgetPolicy(max float64, calls *atomic.Int32) InputHandler {
	return func(_ context.Context, _ string, in agentproto.InputRequest) (string, bool, error) {
		calls.Add(1)
		if in.Kind != agentproto.InputBudget {
			return "", false, nil
		}
		if in.BudgetUSD+in.ExtensionUSD > max {
			return agentproto.ChoiceStop, true, nil
		}
		return in.Choices[0], true, nil
	}
}

func TestInputHandlerAnswersAsk(t *testing.T) {
	var seen agentproto.InputRequest
	e, _, workspace := handlerEngine(t, askThenDone, WithInputHandler(func(_ context.Context, _ string, in agentproto.InputRequest) (string, bool, error) {
		seen = in
		return "main.go", true, nil
	}))
	res, err := e.Run(context.Background(), handlerRequest(workspace), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("Run = %+v, want the final outcome", res)
	}
	if seen.Kind != agentproto.InputAsk || seen.Question != "Which file?" {
		t.Fatalf("handler saw %+v", seen)
	}
}

func TestInputHandlerDeclinesPausesAsUsual(t *testing.T) {
	e, _, workspace := handlerEngine(t, askThenDone, WithInputHandler(func(context.Context, string, agentproto.InputRequest) (string, bool, error) {
		return "", false, nil
	}))
	res, _ := e.Run(context.Background(), handlerRequest(workspace), nil)
	if res.Status != agentproto.InputRequired {
		t.Fatalf("Run = %+v", res)
	}
	if input, _ := res.Result["input"].(agentproto.InputRequest); input.ID == "" {
		t.Fatalf("no pending input in %+v", res)
	}
}

func TestInputHandlerErrorPauses(t *testing.T) {
	e, _, workspace := handlerEngine(t, askThenDone, WithInputHandler(func(context.Context, string, agentproto.InputRequest) (string, bool, error) {
		return "x", true, errors.New("boom")
	}))
	res, _ := e.Run(context.Background(), handlerRequest(workspace), nil)
	if res.Status != agentproto.InputRequired {
		t.Fatalf("Run = %+v, a handler error must pause", res)
	}
}

func TestInputHandlerRejectedAnswerPauses(t *testing.T) {
	reply := func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("c1", "ask", map[string]any{"question": "Which?", "choices": []any{"a", "b"}, "allow_freeform": false})
		}
		return responsesText("done")
	}
	e, _, workspace := handlerEngine(t, reply, WithInputHandler(func(context.Context, string, agentproto.InputRequest) (string, bool, error) {
		return "c", true, nil
	}))
	res, _ := e.Run(context.Background(), handlerRequest(workspace), nil)
	if res.Status != agentproto.InputRequired {
		t.Fatalf("Run = %+v, an answer that is not a choice must pause", res)
	}
}

func TestInputHandlerExtendsBudgetWithinCap(t *testing.T) {
	var calls atomic.Int32
	e, st, workspace := handlerEngine(t, func(int, map[string]any) map[string]any { return responsesText("done") }, WithInputHandler(budgetPolicy(5, &calls)))
	res := overBudget(t, e, st, workspace, 1.5)
	if res.Status != agentproto.Pass || calls.Load() != 1 {
		t.Fatalf("result = %+v after %d handler calls", res, calls.Load())
	}
	ss, _ := st.Sessions(context.Background())
	if len(ss) != 1 || ss[0].BudgetUSD != 2 {
		t.Fatalf("sessions = %+v, want budget extended once", ss)
	}
	// The choice is a harness note, never the latest request.
	if latest, _ := st.LatestRequest(context.Background(), ss[0].ID); latest != "go on" {
		t.Fatalf("latest_request = %q, want the person's own message", latest)
	}
}

func TestInputHandlerStopsAtBudgetCap(t *testing.T) {
	var calls atomic.Int32
	e, st, workspace := handlerEngine(t, func(int, map[string]any) map[string]any { return responsesText("done") }, WithInputHandler(budgetPolicy(1.5, &calls)))
	res := overBudget(t, e, st, workspace, 1.5)
	if res.Status != agentproto.Cancelled || calls.Load() != 1 {
		t.Fatalf("result = %+v after %d handler calls, want the session stopped", res, calls.Load())
	}
}

// A handler that always agrees to extend must not loop: it is asked at most
// the cap, then the session pauses for a person.
func TestInputHandlerAnswerCap(t *testing.T) {
	var calls atomic.Int32
	e, st, workspace := handlerEngine(t, func(int, map[string]any) map[string]any { return responsesText("done") }, WithMaxAutoAnswers(3), WithInputHandler(budgetPolicy(1e9, &calls)))
	// Spending 100 against a budget extended $1 at a time never catches up.
	res := overBudget(t, e, st, workspace, 100)
	if res.Status != agentproto.InputRequired || calls.Load() != 3 {
		t.Fatalf("result = %+v after %d handler calls, want a pause after 3", res, calls.Load())
	}
}
