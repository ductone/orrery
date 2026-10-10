package core

import (
	"context"
	"encoding/json"
	"github.com/ductone/orrey/internal/classify"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
		{"moderate", "implement", 0.5, 200, router.Implement, "jev"},
		{"no better than chance", "implement", 0.15, 200, router.Plan, "default"},
		{"classifier down", "", 0, 529, router.Plan, "default"},
	} {
		got := phaseEngine(t, tc.choice, tc.confidence, tc.status).instructionPhase(ctx, s, stored, nil)
		if got.Phase != tc.want || got.Source != tc.source {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
	unsure := phaseEngine(t, "implement", 0.15, 200).instructionPhase(ctx, s, stored, nil)
	if unsure.Suggested != router.Implement || unsure.Confidence != 0.15 {
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
	e, _ := testEngine(t)
	workspace, git := gitRepo(t)
	writeFile(t, workspace, "README.md", "base\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	reviews, turns := 0, 0
	s := &scriptedResponses{reply: func(_ int, body map[string]any) map[string]any {
		if strings.Contains(body["instructions"].(string), "Review this proposed workspace diff") {
			reviews++
			if reviews == 1 {
				return verdictJSON(false, "main.go:1 writes the wrong content")
			}
			return verdictJSON(true)
		}
		turns++
		switch turns {
		case 1:
			return responsesCall("w1", "exec", map[string]any{"command": "printf 'content\\n' > main.go"})
		case 3:
			return responsesCall("w2", "exec", map[string]any{"command": "printf 'fixed\\n' > main.go"})
		}
		return responsesText("Done: wrote main.go")
	}}
	srv := s.serve(t)
	cfg := gateConfig(workspace, srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "Write main.go", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	sid, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Status != agentproto.Pass {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the run must end")
	}
	var nudged bool
	for _, req := range s.requests {
		for _, raw := range req["input"].([]any) {
			m, _ := raw.(map[string]any)
			c, _ := m["content"].(string)
			if strings.Contains(c, "Independent review rejected completion") {
				nudged = true
			}
		}
	}
	if !nudged {
		t.Fatal("the scenario must include a harness review rejection")
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

func TestFollowUpResetsStoredPhase(t *testing.T) {
	e := phaseEngine(t, "implement", 0.95, 200)
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n <= 6 {
			return responsesCall("r"+strconv.Itoa(n), "read", map[string]any{"path": "README"})
		}
		return responsesText("done")
	}}
	srv := s.serve(t)
	cfg, _, _, _, _ := e.runtimeSnapshot()
	workspace := t.TempDir()
	cfg.WorkspaceRoot = workspace
	cfg.Providers = map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}
	cfg.Router = config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := e.store.CreateSession(ctx, store.Session{ID: sid, Spec: "build the feature", Phase: "review", BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
		t.Fatal(err)
	}
	_ = e.store.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "The previous task is reviewed."})
	_ = e.store.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Content: "now implement the next part"})
	req := agentproto.TaskRequest{Spec: "build the feature", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	if r := e.run(ctx, sid, "", req, nil); r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	got, err := e.store.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != string(router.Implement) {
		t.Fatalf("session phase = %q, want implement", got.Phase)
	}
	states := routingStates(t, e, sid)
	if len(states) < 6 {
		t.Fatalf("turns = %d, want at least 6", len(states))
	}
	for _, state := range states {
		if state.Phase == router.Review || state.Phase == router.Diagnose {
			t.Fatalf("follow-up inherited review resolution: %+v", state)
		}
	}

}
func TestInstructionPhaseStateCarriesFollowUpContext(t *testing.T) {
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Spec: "build the feature", Phase: "wrap-up"}
	type capture struct {
		state map[string]any
		req   map[string]any
	}
	seen := make(chan capture, 1)
	e, st := testEngine(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     map[string]any `json:"state"`
			Questions map[string]any `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen <- capture{state: req.State, req: map[string]any{"questions": req.Questions}}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"phase": map[string]any{"type": "choice", "choice": "implement", "confidence": 0.95}}})
	}))
	t.Cleanup(srv.Close)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Routing: true}}, nil, nil)
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTodos(ctx, s.ID, []store.Todo{{Text: "audit summaries", Phase: "review", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	stored := storedMessages(
		provider.Message{Role: "user", Content: "build the feature"},
		provider.Message{Role: "assistant", Content: "Done: the feature is built and tested."},
		provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read"}}, Content: "calling read"},
		provider.Message{Role: "user", Content: "also update the docs"},
	)
	choice := e.instructionPhase(ctx, s, stored, nil)
	if choice.QuestionVersion != instructionPhaseVersion {
		t.Fatalf("question version = %q, want %q", choice.QuestionVersion, instructionPhaseVersion)
	}
	cap := <-seen
	if got := cap.state["previous_answer"]; got != "Done: the feature is built and tested." {
		t.Errorf("previous_answer = %v", got)
	}
	if got := cap.state["first_request"]; got != "build the feature" {
		t.Errorf("first_request = %v", got)
	}
	raw, ok := cap.state["previous_plan"].([]any)
	item, _ := raw[0].(map[string]any)
	if !ok || len(raw) != 1 || item["text"] != "audit summaries" || item["status"] != "completed" {
		t.Errorf("previous_plan = %#v", cap.state["previous_plan"])
	}
	q, _ := json.Marshal(cap.req["questions"])
	if !strings.Contains(string(q), "recommendation or explanation") {
		t.Error("the explore criterion must cover questions, recommendations, and explanations")
	}
}

func TestInstructionPhaseUsesThePluggedClassifier(t *testing.T) {
	e, _ := testEngine(t)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{Routing: true}}, nil, nil)
	fake := &classify.Fake{Answer: func(_ any, name string, _ classify.Question) (classify.Answer, error) {
		return classify.Answer{Type: "choice", Choice: "diagnose", Probabilities: map[string]float64{"diagnose": .4, "plan": .3}}, nil
	}}
	e.UseClassifier(fake)
	s := store.Session{ID: uuid.NewString(), Spec: "build the feature", Phase: "implement"}
	got := e.instructionPhase(context.Background(), s, storedMessages(provider.Message{Role: "user", Content: "the tests fail now"}), nil)
	if got.Phase != router.Diagnose || got.Confidence != .4 || len(fake.Calls) != 1 {
		t.Fatalf("choice = %+v calls=%d", got, len(fake.Calls))
	}
}
