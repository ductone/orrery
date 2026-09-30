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

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/shadow"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

// fakeJev answers every question with a fixed, type-appropriate answer and
// records the questions it was asked. A non-200 status fails every call.
func fakeJev(t *testing.T, status int) (string, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var asked atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			State     any                        `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		asked.Store(req.Questions)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		answers := map[string]any{}
		for name, raw := range req.Questions {
			var q struct{ Type string }
			_ = json.Unmarshal(raw, &q)
			switch q.Type {
			case "noul":
				answers[name] = map[string]any{"type": "noul", "noul": 0.9}
			case "choice":
				answers[name] = map[string]any{"type": "choice", "choice": "discipline", "confidence": 0.7, "probabilities": map[string]float64{"discipline": 0.8}}
			case "score":
				answers[name] = map[string]any{"type": "score", "score": 2.0, "confidence": 0.5, "legend": map[string]string{"0": "a", "1": "b", "2": "c", "3": "d"}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100}})
	}))
	t.Cleanup(server.Close)
	return server.URL, &calls, &asked
}

func enableShadow(e *Engine, url string, sites ...string) {
	cfg, providers, _, mc, _ := e.runtimeSnapshot()
	cfg.Jev.APIKey, cfg.Jev.BaseURL, cfg.Jev.Shadow = "k", url, sites
	e.ReplaceRuntime(cfg, providers, mc)
}

func shadowRecords(t *testing.T, e *Engine, st *store.Store) []store.ShadowRecord {
	t.Helper()
	e.waitShadows()
	recs, err := st.ShadowRecords(context.Background(), time.Time{}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestStallJudgeShadowRecordsJudgeVerdictAsBaseline(t *testing.T) {
	e, st, _ := judgeEngine(t, `"{\"intervene\":false,\"reason\":\"searching new terms\"}"`, 200)
	url, calls, _ := fakeJev(t, 200)
	enableShadow(e, url, "stall_judge")
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	if e.allowIntervention(context.Background(), sid, "escalation", "no_progress_turns", 4, p, nil) {
		t.Fatal("the LLM judge declined; the shadow must not change that")
	}
	recs := shadowRecords(t, e, st)
	if len(recs) != 1 || calls.Load() != 1 {
		t.Fatalf("records=%d calls=%d", len(recs), calls.Load())
	}
	r := recs[0]
	if r.Site != shadow.StallJudge || r.QuestionVersion != shadow.StallJudgeVersion || r.ClassifierModel != "jev-1.13.0" {
		t.Fatalf("record = %+v", r)
	}
	if !strings.Contains(string(r.State), "topbar") || !strings.Contains(string(r.Baseline), `"intervene":false`) {
		t.Fatalf("state=%s baseline=%s", r.State, r.Baseline)
	}
	checks := shadow.Checks(r)
	if len(checks) != 1 || checks[0].Agree {
		t.Fatalf("jev said stuck (0.9) and the judge declined; checks = %+v", checks)
	}
}

func TestShadowFailureIsInert(t *testing.T) {
	e, st, _ := judgeEngine(t, `"{\"intervene\":true,\"reason\":\"repeating\"}"`, 200)
	url, _, _ := fakeJev(t, 529)
	enableShadow(e, url, "stall_judge")
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	if !e.allowIntervention(context.Background(), sid, "escalation", "no_progress_turns", 4, p, nil) {
		t.Fatal("a failing shadow must not change the judge's verdict")
	}
	recs := shadowRecords(t, e, st)
	if len(recs) != 1 || !strings.Contains(recs[0].Error, "529") || recs[0].Answers != "" {
		t.Fatalf("records = %+v", recs)
	}
	if s, _ := st.Session(context.Background(), sid); s.SpentUSD <= 0 {
		t.Fatal("the LLM judge's spend must still be recorded")
	}
}

func TestShadowOffByDefault(t *testing.T) {
	e, st, _ := judgeEngine(t, `"{\"intervene\":true,\"reason\":\"repeating\"}"`, 200)
	_, calls, _ := fakeJev(t, 200)
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	e.allowIntervention(context.Background(), sid, "escalation", "no_progress_turns", 4, p, nil)
	e.shadowTurn(context.Background(), store.Session{ID: sid, Spec: "x"}, nil, router.RoutingState{Turn: 1}, router.Decision{})
	if recs := shadowRecords(t, e, st); len(recs) != 0 || calls.Load() != 0 {
		t.Fatalf("records=%d calls=%d", len(recs), calls.Load())
	}
}

func TestShadowTurnAsksOnlyEnabledQuestions(t *testing.T) {
	e, st := testEngine(t)
	url, _, asked := fakeJev(t, 200)
	enableShadow(e, url, "phase")
	ctx := context.Background()
	sid := uuid.NewString()
	s := store.Session{ID: sid, Spec: "fix the flaky test", Phase: "implement", BudgetUSD: 5}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	_ = st.SetTodos(ctx, sid, []store.Todo{{Text: "fix it", Phase: "implement", Status: "in_progress"}})
	_ = st.AddMessage(ctx, sid, "assistant", provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "1", Name: "exec", Arguments: map[string]any{"command": "go test ./..."}}}})
	stored, _ := st.Messages(ctx, sid)
	state := router.RoutingState{Turn: 4, Point: router.TurnStart, Phase: router.Implement}
	e.shadowTurn(ctx, s, stored, state, router.Decision{Model: model.ModelSpec{ID: "m", Tier: model.Efficient}, Effort: model.EffortMedium})
	recs := shadowRecords(t, e, st)
	if len(recs) != 1 || recs[0].Site != shadow.Turn || recs[0].Turn != 4 {
		t.Fatalf("records = %+v", recs)
	}
	questions := asked.Load().(map[string]json.RawMessage)
	if _, ok := questions["phase"]; !ok || len(questions) != 1 {
		t.Fatalf("difficulty is not enabled; asked %v", questions)
	}
	for _, want := range []string{`"routed_phase":"implement"`, `"tier":"efficient"`} {
		if !strings.Contains(string(recs[0].Baseline), want) {
			t.Fatalf("baseline %s missing %s", recs[0].Baseline, want)
		}
	}
	if !strings.Contains(string(recs[0].State), "go test") || !strings.Contains(string(recs[0].State), "fix it") {
		t.Fatalf("state = %s", recs[0].State)
	}
}
