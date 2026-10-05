package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestTaskSectionLeadsWithTheLatestRequest(t *testing.T) {
	if got := taskSection("Build it", ""); !strings.HasPrefix(got, "CURRENT REQUEST (the person's latest message; authoritative)\nBuild it") {
		t.Fatalf("no follow-up uses the first request: %q", got)
	}
	got := taskSection("Is there a way to resume a session?", "Build it")
	if !strings.HasPrefix(got, "CURRENT REQUEST") || strings.Index(got, "Build it") > strings.Index(got, "resume") || !strings.Contains(got, "already answered unless") {
		t.Fatalf("section = %q", got)
	}
	state := DurableState{Objective: "x", CurrentObjective: "Implement the schema", PendingReport: "report", ResolvedRequests: []string{"resume question answered"}}
	s := store.Session{Spec: "Is there a way to resume a session?", DurableSummary: store.JSON(state)}
	spec := durableSpec(s)
	if strings.Contains(spec, "Implement the schema") || strings.Contains(spec, `"pending_report":"report"`) || !strings.Contains(spec, "resume question answered") {
		t.Fatalf("durable spec = %q", spec)
	}
	if got := currentRequest(s, "Build it"); !strings.HasPrefix(got, "CURRENT REQUEST (the person's latest message; authoritative)\nBuild it") || !strings.Contains(got, "Implement the schema") || !strings.Contains(got, "PENDING REPORT") {
		t.Fatalf("current request = %q", got)
	}
	state.CurrentObjective, state.PendingReport = "", ""
	s.DurableSummary = store.JSON(state)
	if got := durableSpec(s); got != spec {
		t.Fatalf("acknowledging a report changed the cached prefix:\n%s\n%s", spec, got)
	}
}

func TestStoreTracksTheLatestRequest(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "first question", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.LatestRequest(ctx, sid); latest != "" {
		t.Fatalf("no follow-up yet: %q", latest)
	}
	if _, err := st.AcceptMessage(ctx, sid, "r1", "t1", "message", "h1", provider.Message{Role: "user", Content: "Build it"}, nil); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.LatestRequest(ctx, sid); latest != "Build it" {
		t.Fatalf("latest = %q", latest)
	}
	// Harness messages do not go through acceptance and never replace it.
	_ = st.AddMessage(ctx, sid, "user", provider.Message{Role: "user", Harness: true, Content: "Completion rejected"})
	if latest, _ := st.LatestRequest(ctx, sid); latest != "Build it" {
		t.Fatalf("latest = %q", latest)
	}
	_ = e
}

func TestWithoutRequest(t *testing.T) {
	resolved := []string{
		"TUI session resume question answered: use orrery tui --session.",
		"User explicitly requested: Build it.",
		"Build it",
		"Previous-session listing question answered.",
	}
	got := withoutRequest(resolved, "Build it")
	if len(got) != 2 || strings.Contains(strings.Join(got, "|"), "Build it") {
		t.Fatalf("got %v", got)
	}
	// A very short request matches only exactly.
	if got := withoutRequest([]string{"ok, the user said ok to the plan", "ok"}, "ok"); len(got) != 1 || got[0] != "ok, the user said ok to the plan" {
		t.Fatalf("short request swept too much: %v", got)
	}
	if got := withoutRequest(resolved, ""); len(got) != len(resolved) {
		t.Fatal("no request, nothing dropped")
	}
}

func TestCompactionNeverResolvesTheLatestRequest(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	prior := store.JSON(DurableState{Objective: "x", CurrentObjective: "c", PendingReport: "p", ResolvedRequests: []string{"Is there a way to resume a session?", "User explicitly requested: Build it."}})
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "Is there a way to resume a session?", Phase: "implement", BudgetUSD: 5, DurableSummary: prior, WorkspacePath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptMessage(ctx, sid, "r1", "t1", "message", "h1", provider.Message{Role: "user", Content: "Build it"}, nil); err != nil {
		t.Fatal(err)
	}
	_ = st.SetTodos(ctx, sid, []store.Todo{{Text: "Wire the catalog", Phase: "implement", Status: "in_progress"}})
	for i := range 12 {
		_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "step " + string(rune('a'+i))})
	}
	if err := e.Compact(ctx, sid, "phase_or_context_boundary", nil); err != nil {
		t.Fatal(err)
	}
	s, _ := st.Session(ctx, sid)
	var state DurableState
	if err := json.Unmarshal([]byte(s.DurableSummary), &state); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(state.ResolvedRequests, "|")
	if strings.Contains(joined, "Build it") || !strings.Contains(joined, "resume") {
		t.Fatalf("resolved = %v", state.ResolvedRequests)
	}
}

// The broken session's shape: the first request was answered, a later one
// asked for work, and the agent answered the first again. The prompt now
// leads with the latest request, and Jev refuses the stale answer.
func TestStaleAnswerToAnEarlierRequestIsRefused(t *testing.T) {
	e, st := testEngine(t)
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State map[string]any `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := 0.9
		if strings.Contains(req.State["final_result"].(string), "resuming") {
			p = 0.05
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"addresses_request": map[string]any{"type": "noul", "noul": p}}})
	}))
	t.Cleanup(jevSrv.Close)
	var instructions []string
	var tails []string
	nudged := false
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		instructions = append(instructions, body["instructions"].(string))
		input := body["input"].([]any)
		tail := input[len(input)-1].(map[string]any)
		tails = append(tails, tail["content"].(string))
		for _, raw := range body["input"].([]any) {
			if m, _ := raw.(map[string]any); m != nil {
				if c, _ := m["content"].(string); strings.Contains(c, "does not address the person's latest request") && strings.Contains(c, "Explain the build plan") {
					nudged = true
				}
			}
		}
		if n == 1 {
			return responsesText("## Answer: resuming a session in the TUI\nUse orrery tui --session ID.")
		}
		return responsesText("Here is the build plan: discover, merge, override.")
	}}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
		Jev:           config.JevConfig{APIKey: "k", BaseURL: jevSrv.URL, Review: true},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	ctx := context.Background()
	sid := uuid.NewString()
	summary := store.JSON(DurableState{Objective: "resume sessions", CurrentObjective: "Explain the build plan", PendingReport: "Report the build plan"})
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "Is there a way to resume a session in the TUI?", DurableSummary: summary, Phase: "plan", BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
		t.Fatal(err)
	}
	_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", Content: "Yes: orrery tui --session ID."})
	if _, err := st.AcceptMessage(ctx, sid, "r1", "t1", "message", "h1", provider.Message{Role: "user", Content: "Explain the build plan"}, nil); err != nil {
		t.Fatal(err)
	}
	req := agentproto.TaskRequest{Spec: "Is there a way to resume a session in the TUI?", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	result := e.run(ctx, sid, "", req, nil)
	if result.Status != agentproto.Pass || !strings.Contains(result.Result["answer"].(string), "build plan") {
		t.Fatalf("result = %+v", result)
	}
	if !nudged || result.Outcome.CompletionRejects != 1 {
		t.Fatalf("the stale answer must be refused once, quoting the latest request: nudged=%v rejects=%d", nudged, result.Outcome.CompletionRejects)
	}
	if !strings.HasPrefix(tails[0], "CURRENT REQUEST (the person's latest message; authoritative)\nExplain the build plan") || !strings.Contains(tails[0], "PENDING REPORT") {
		t.Fatal("the conversation tail must prominently carry the latest request and report")
	}
	if _, err := st.AcceptMessage(ctx, sid, "r2", "t2", "message", "h2", provider.Message{Role: "user", Content: "Summarize the build plan"}, nil); err != nil {
		t.Fatal(err)
	}
	result = e.run(ctx, sid, "", req, nil)
	if result.Status != agentproto.Pass {
		t.Fatalf("follow-up result = %+v", result)
	}
	for _, got := range instructions {
		if got != instructions[0] {
			t.Fatalf("follow-up changed system sections:\n%s\n%s", instructions[0], got)
		}
	}
	if strings.Contains(instructions[0], "CURRENT REQUEST") || strings.Contains(instructions[0], "Report the build plan") {
		t.Fatal("volatile request/report leaked into system sections")
	}
	if !strings.HasPrefix(tails[len(tails)-1], "CURRENT REQUEST (the person's latest message; authoritative)\nSummarize the build plan") || strings.Contains(tails[len(tails)-1], "PENDING REPORT") {
		t.Fatal("follow-up tail must carry the new request without the delivered report")
	}
}

func TestAnswerCheckFailsOpenAndIsBounded(t *testing.T) {
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Spec: "first"}
	e, _ := testEngine(t)
	if _, off := e.answerOffTopic(ctx, s.ID, s, "Build it", "anything", nil); off {
		t.Fatal("without jev.review there is no check")
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(529) }))
	t.Cleanup(down.Close)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: down.URL, Review: true}}, nil, nil)
	if _, off := e.answerOffTopic(ctx, s.ID, s, "Build it", "anything", nil); off {
		t.Fatal("a classifier outage must not refuse completion")
	}
	if maxAnswerRejections < 1 || maxAnswerRejections > 3 {
		t.Fatalf("the refusal bound must stay small: %d", maxAnswerRejections)
	}
}

func TestAnswerCheckSendsTodoPlanForPointerRequest(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	s := store.Session{ID: uuid.NewString(), Spec: "Explain how sessions work", BudgetUSD: 1}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTodos(ctx, s.ID, []store.Todo{{Text: "Old plan", Phase: "explore", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTodos(ctx, s.ID, []store.Todo{
		{Text: "Implement session resume in the TUI", Phase: "implement", Status: "completed"},
		{Text: "Verify session resume", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		State map[string]string `json:"state"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"addresses_request": map[string]any{"type": "noul", "noul": 0.9}}})
	}))
	t.Cleanup(srv.Close)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Review: true}}, nil, nil)
	request := "Implement bead orrery-vau"
	draft := "Implemented session resume in the TUI and verified it."
	if got, off := e.answerOffTopic(ctx, s.ID, s, request, draft, nil); got != request || off {
		t.Fatalf("request=%q off_topic=%v", got, off)
	}
	if body.State["latest_request"] != request || body.State["final_result"] != draft {
		t.Fatalf("state = %v", body.State)
	}
	if got := body.State["todo_plan"]; got != "completed: Implement session resume in the TUI\ncompleted: Verify session resume" {
		t.Fatalf("todo_plan = %q", got)
	}
	if _, ok := body.State["earlier_request"]; ok {
		t.Fatal("the earlier request must not be sent to Jev")
	}
}

func TestAnswerAnnouncementFollowsOpenTodos(t *testing.T) {
	const announcement = "The bead is closed. Now I'll start orrery-qqb and inspect the issue plus rate-limit handling."
	const report = "Implemented the rate-limit fix, verified it, and closed the bead."
	for _, tc := range []struct {
		name, status string
		wantQuestion bool
		wantReason   bool
	}{
		{"announces unfinished work", "pending", true, true},
		{"reports despite stale todos", "pending", true, false},
		{"completed todos need no announcement question", "completed", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e, st := testEngine(t)
			s := store.Session{ID: uuid.NewString(), Spec: "Implement orrery-qqb", BudgetUSD: 1}
			if err := st.CreateSession(ctx, s); err != nil {
				t.Fatal(err)
			}
			if err := st.SetTodos(ctx, s.ID, []store.Todo{{Text: "Inspect rate-limit handling", Status: tc.status}}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			sawQuestion := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					Questions map[string]any    `json:"questions"`
					State     map[string]string `json:"state"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				_, sawQuestion = req.Questions["reports_completed_work"]
				p := 0.9
				if strings.Contains(req.State["final_result"], "Now I'll start") {
					p = 0.05
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
					"addresses_request":      map[string]any{"type": "noul", "noul": 0.81},
					"reports_completed_work": map[string]any{"type": "noul", "noul": p},
				}})
			}))
			t.Cleanup(srv.Close)
			e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Review: true}}, nil, nil)
			result := announcement
			if !tc.wantReason {
				result = report
			}
			_, _, reason := e.checkAnswer(ctx, s.ID, s, s.Spec, result, nil)
			if calls != 1 || sawQuestion != tc.wantQuestion || (reason != "") != tc.wantReason {
				t.Fatalf("calls=%d question=%v reason=%q", calls, sawQuestion, reason)
			}
		})
	}
	ctx := context.Background()
	e, _ := testEngine(t)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: "http://127.0.0.1:1", Review: true}}, nil, nil)
	s := store.Session{ID: uuid.NewString(), Spec: "Implement orrery-qqb"}
	if _, _, reason := e.checkAnswer(ctx, s.ID, s, s.Spec, announcement, nil); reason != "" {
		t.Fatal("a classifier outage must not reject an announcement")
	}
}

func TestWorkerAnswerIsCheckedAgainstItsSpec(t *testing.T) {
	for _, alwaysOffTopic := range []bool{false, true} {
		name := "recovers"
		if alwaysOffTopic {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			e, st := testEngine(t)
			const spec = "Explain the provider classification"
			const answer = "Provider classification distinguishes transport errors from model refusals."
			checks := 0
			jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					State map[string]string `json:"state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				checks++
				if req.State["latest_request"] != spec {
					t.Errorf("checked request = %q, want spec %q", req.State["latest_request"], spec)
				}
				p := 0.05
				if !alwaysOffTopic && req.State["final_result"] == answer {
					p = 0.9
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"addresses_request": map[string]any{"type": "noul", "noul": p}}})
			}))
			t.Cleanup(jevSrv.Close)
			nudged := false
			turns := 0
			s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
				turns = n
				for _, raw := range body["input"].([]any) {
					if m, _ := raw.(map[string]any); m != nil {
						if c, _ := m["content"].(string); strings.Contains(c, "does not address the assigned task (the spec)") && strings.Contains(c, spec) && !strings.Contains(c, "person's latest request") {
							nudged = true
						}
					}
				}
				if n == 1 || alwaysOffTopic {
					return responsesText("<invoke name=\"read\">internal/core/limits.go</invoke>")
				}
				return responsesText(answer)
			}}
			srv := s.serve(t)
			workspace := t.TempDir()
			cfg := config.Config{
				WorkspaceRoot: workspace,
				Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}},
				Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
				Jev:           config.JevConfig{APIKey: "k", BaseURL: jevSrv.URL, Review: true},
			}
			e.ReplaceRuntime(cfg, provider.New(cfg), nil)
			ctx := context.Background()
			sid := uuid.NewString()
			if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: spec, Phase: "implement", BudgetUSD: 5, WorkspacePath: workspace}); err != nil {
				t.Fatal(err)
			}
			// Even a stored follow-up cannot replace the worker's assigned spec.
			if _, err := st.AcceptMessage(ctx, sid, "r1", "t1", "message", "h1", provider.Message{Role: "user", Content: "Explain session resume"}, nil); err != nil {
				t.Fatal(err)
			}
			req := agentproto.TaskRequest{Spec: spec, Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
			result := e.run(ctx, sid, "worker-job", req, nil)
			wantRejects, wantChecks := 1, 2
			if alwaysOffTopic {
				wantRejects, wantChecks = maxAnswerRejections, maxAnswerRejections
			}
			if result.Status != agentproto.Pass || !nudged || result.Outcome.CompletionRejects != wantRejects || checks != wantChecks || turns != wantRejects+1 {
				t.Fatalf("result=%+v nudged=%v checks=%d turns=%d", result, nudged, checks, turns)
			}
			if !alwaysOffTopic && result.Result["answer"] != answer {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}
