package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func storedMessages(msgs ...provider.Message) []store.Message {
	out := make([]store.Message, len(msgs))
	for i, m := range msgs {
		out[i] = store.Message{Role: m.Role, ContentJSON: store.JSON(m)}
	}
	return out
}

func TestEndsWithUserInstruction(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []store.Message
		want bool
	}{
		{"empty", nil, false},
		{"person", storedMessages(provider.Message{Role: "user", Content: "also fix the typo"}), true},
		{"harness nudge", storedMessages(provider.Message{Role: "user", Content: "Completion rejected", Harness: true}), false},
		{"assistant last", storedMessages(provider.Message{Role: "user", Content: "go"}, provider.Message{Role: "assistant", Content: "ok"}), false},
		{"tool last", storedMessages(provider.Message{Role: "tool", Content: "{}"}), false},
		{"legacy message without the flag", []store.Message{{Role: "user", ContentJSON: `{"role":"user","content":"hi"}`}}, true},
	} {
		if got := endsWithUserInstruction(tc.msgs); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := lastUserText(storedMessages(provider.Message{Role: "user", Content: "real ask"}, provider.Message{Role: "user", Content: "nudge", Harness: true})); got != "real ask" {
		t.Fatalf("lastUserText = %q", got)
	}
}

func phaseEngine(t *testing.T, choice string, confidence float64, status int) *Engine {
	t.Helper()
	e, st := testEngine(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		var req struct {
			State map[string]any `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.State["message"] == "" {
			t.Error("the classifier must see the user's message")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"phase": map[string]any{"type": "choice", "choice": choice, "confidence": confidence}}})
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Routing: true}}
	e.ReplaceRuntime(cfg, nil, nil)
	_ = st
	return e
}

func TestInstructionPhase(t *testing.T) {
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Spec: "build the feature", Phase: "implement"}
	stored := storedMessages(provider.Message{Role: "user", Content: "keep going"})
	for _, tc := range []struct {
		name       string
		choice     string
		confidence float64
		status     int
		want       router.Phase
		source     string
	}{
		{"confident continuation", "implement", 0.92, 200, router.Implement, "jev"},
		{"confident new request", "plan", 0.9, 200, router.Plan, "jev"},
		{"unsure", "implement", 0.5, 200, router.Plan, "default"},
		{"classifier down", "", 0, 529, router.Plan, "default"},
	} {
		got := phaseEngine(t, tc.choice, tc.confidence, tc.status).instructionPhase(ctx, s, stored, nil)
		if got.Phase != tc.want || got.Source != tc.source {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
	unsure := phaseEngine(t, "implement", 0.5, 200).instructionPhase(ctx, s, stored, nil)
	if unsure.Suggested != router.Implement || unsure.Confidence != 0.5 {
		t.Fatalf("a declined suggestion is still recorded: %+v", unsure)
	}
	e, _ := testEngine(t)
	if got := e.instructionPhase(ctx, s, stored, nil); got.Phase != router.Plan || got.Source != "default" {
		t.Fatalf("without jev.routing a new instruction is planned: %+v", got)
	}
}

// routingStates returns the recorded routing state of each turn decision.
func routingStates(t *testing.T, e *Engine, sid string) []router.RoutingState {
	t.Helper()
	rows, err := e.store.DB().Query(`SELECT state_json FROM routing_records WHERE session_id=? AND decision_point!='review' ORDER BY created_at`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []router.RoutingState
	for rows.Next() {
		var raw string
		_ = rows.Scan(&raw)
		var s router.RoutingState
		_ = json.Unmarshal([]byte(raw), &s)
		out = append(out, s)
	}
	return out
}

func TestHarnessNudgesAreNotNewInstructions(t *testing.T) {
	result, _, e, sid := gateRun(t, "main.go", "", -1)
	if result.Outcome.CompletionRejects == 0 {
		t.Fatal("the scenario must include harness rejections")
	}
	for _, s := range routingStates(t, e, sid) {
		if s.NewInstruction || s.Phase == router.Plan && s.InstructionPhase != nil {
			t.Fatalf("turn %d treated a harness nudge as a new instruction: %+v", s.Turn, s)
		}
	}
}

func TestUserFollowUpIsRoutedByJev(t *testing.T) {
	e := phaseEngine(t, "implement", 0.95, 200)
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	srv := s.serve(t)
	cfg, _, _, _, _ := e.runtimeSnapshot()
	workspace := t.TempDir()
	cfg.WorkspaceRoot = workspace
	cfg.Providers = map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}
	cfg.Router = config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"}
	cfg.Interventions = config.InterventionConfig{JudgeEnabled: new(bool)}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := e.store.CreateSession(ctx, store.Session{ID: sid, Spec: "build the feature", Phase: "implement", BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
		t.Fatal(err)
	}
	_ = e.store.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "Implemented part one."})
	_ = e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: "keep going with part two"})
	req := agentproto.TaskRequest{Spec: "build the feature", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	if r := e.run(ctx, sid, "", req, nil); r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	states := routingStates(t, e, sid)
	if len(states) == 0 || !states[0].NewInstruction || states[0].Phase != router.Implement || states[0].InstructionPhase == nil || states[0].InstructionPhase.Source != "jev" {
		t.Fatalf("first turn state = %+v", states)
	}
}
