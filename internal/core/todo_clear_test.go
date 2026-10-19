package core

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
)

func TestShouldClearTodos(t *testing.T) {
	for _, tc := range []struct {
		status string
		newIns bool
		want   bool
	}{
		{string(agentproto.Pass), true, true},
		{string(agentproto.Pass), false, false},
		{string(agentproto.InputRequired), true, false},
		{"interrupted", true, false},
		{string(agentproto.Cancelled), true, false},
		{string(agentproto.Fail), true, false},
		{"running", true, false},
		{"", true, false},
	} {
		if got := shouldClearTodos(tc.status, tc.newIns); got != tc.want {
			t.Errorf("shouldClearTodos(%q, %v) = %v, want %v", tc.status, tc.newIns, got, tc.want)
		}
	}
}

// continueAfter seeds a session whose previous run ended with prevStatus and
// left a todo plan, sends a new user message through the real launch path
// (which marks the session running first), and returns the plan afterwards.
func continueAfter(t *testing.T, prevStatus string) ([]store.Todo, store.Session, []store.Message) {
	t.Helper()
	e, st := testEngine(t)
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "first task", Phase: "implement", BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
		t.Fatal(err)
	}
	_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "First task done."})
	if err := st.SetTodos(ctx, sid, []store.Todo{{Text: "old step", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, sid, prevStatus); err != nil {
		t.Fatal(err)
	}
	info, err := e.ContinueIntegrated(ctx, sid, "now do the second task", uuid.NewString(), "message", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := <-info.Result; r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	e.waitBackground()
	todos, err := st.Todos(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := st.Messages(ctx, sid)
	return todos, got, msgs
}

func TestNewMessageAfterPassClearsTodoPlan(t *testing.T) {
	todos, s, msgs := continueAfter(t, string(agentproto.Pass))
	if len(todos) != 0 {
		t.Fatalf("todos after a passed run and a new message = %+v, want cleared", todos)
	}
	if !strings.Contains(s.DurableSummary, "[TODO SNAPSHOT]") || !strings.Contains(s.DurableSummary, "old step") {
		t.Fatalf("the cleared plan must be snapshotted, summary = %q", s.DurableSummary)
	}
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.ContentJSON, "Cleared todo plan") {
			t.Fatal("clearing must not add a user-role note that could read as a new instruction")
		}
	}
}

func TestNewMessageKeepsTodoPlanAfterNonPassRun(t *testing.T) {
	for _, status := range []string{string(agentproto.InputRequired), "interrupted", string(agentproto.Cancelled), string(agentproto.Fail)} {
		t.Run(status, func(t *testing.T) {
			todos, _, _ := continueAfter(t, status)
			if len(todos) != 1 || todos[0].Text != "old step" {
				t.Fatalf("todos after a %s run = %+v, want kept", status, todos)
			}
		})
	}
}
