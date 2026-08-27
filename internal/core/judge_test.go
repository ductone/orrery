package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestBackoffIsMultiplicative(t *testing.T) {
	p := newProgressTracker()
	factor := config.Default().Interventions.Backoff()
	if factor != 1.5 {
		t.Fatalf("default backoff = %v, want 1.5", factor)
	}
	// Each declined verdict raises the bar by half again, so the judge is asked
	// a logarithmic number of times rather than once per turn.
	for _, tc := range []struct{ observed, want int }{{3, 5}, {4, 6}, {6, 9}, {9, 14}, {20, 30}} {
		p := newProgressTracker()
		p.backoff("no_progress_turns", tc.observed, factor)
		if got := p.floors["no_progress_turns"]; got != tc.want {
			t.Errorf("backoff(%d) = %d, want %d", tc.observed, got, tc.want)
		}
	}
	// A factor that would not clear the observed value must still advance, or
	// the judge would be re-asked every turn forever.
	p.backoff("repeated_reads", 2, 1.01)
	if got := p.floors["repeated_reads"]; got <= 2 {
		t.Fatalf("floor must exceed the tripping value, got %d", got)
	}
}

func TestThresholdUsesFloorOnlyWhenHigher(t *testing.T) {
	p := newProgressTracker()
	if got := p.threshold("no_progress_turns", 3); got != 3 {
		t.Fatalf("unjudged threshold = %d, want the base 3", got)
	}
	p.backoff("no_progress_turns", 4, 1.5)
	if got := p.threshold("no_progress_turns", 3); got != 6 {
		t.Fatalf("threshold after backoff = %d, want 6", got)
	}
	// A base above the floor still wins: backing off one signal must not make
	// a stricter trigger more eager.
	if got := p.threshold("no_progress_turns", 9); got != 9 {
		t.Fatalf("threshold = %d, want the higher base 9", got)
	}
}

// Floors are memoised verdicts about a phase's normal working depth, so they
// expire when the phase does — the same reset the counters already get.
func TestFloorsResetOnPhaseChange(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("explore")
	p.backoff("no_progress_turns", 4, 1.5)
	p.beginTurn("explore")
	if p.floors["no_progress_turns"] != 6 {
		t.Fatal("floor must survive within a phase")
	}
	p.beginTurn("implement")
	if len(p.floors) != 0 {
		t.Fatalf("floors must clear on phase change, got %v", p.floors)
	}
}

func TestEscalationTriggerNamesTheTrippingClause(t *testing.T) {
	p := newProgressTracker()
	signal, observed, tripped := escalationTrigger(router.StallSignals{NoProgressTurns: 4}, p)
	if !tripped || signal != "no_progress_turns" || observed != 4 {
		t.Fatalf("got %q/%d/%v", signal, observed, tripped)
	}
	// After backing that clause off, the same value no longer trips it...
	p.backoff("no_progress_turns", 4, 1.5)
	if _, _, tripped := escalationTrigger(router.StallSignals{NoProgressTurns: 4}, p); tripped {
		t.Fatal("backed-off clause must not trip at the same value")
	}
	// ...but a different clause is unaffected, since floors are per-signal.
	signal, _, tripped = escalationTrigger(router.StallSignals{NoProgressTurns: 4, RepeatedReads: 2}, p)
	if !tripped || signal != "repeated_reads" {
		t.Fatalf("independent clause suppressed: %q %v", signal, tripped)
	}
}

func TestParseJudgeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantOK    bool
		intervene bool
	}{
		{"plain", `{"intervene":true,"reason":"looping"}`, true, true},
		{"fenced", "```json\n{\"intervene\":false,\"reason\":\"new files\"}\n```", true, false},
		{"prose wrapped", `Here: {"intervene":false,"reason":"fine"} hope that helps`, true, false},
		{"not json", "I think it is stuck", false, false},
		{"empty", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := parseJudgeVerdict(tc.body)
			if ok != tc.wantOK || v.Intervene != tc.intervene {
				t.Fatalf("got %+v/%v want intervene=%v ok=%v", v, ok, tc.intervene, tc.wantOK)
			}
		})
	}
}

// judgeEngine wires an engine to a stub provider that returns the given verdict
// body for every request, and returns the number of calls made.
func judgeEngine(t *testing.T, body string, status int) (*Engine, *store.Store, *atomic.Int32) {
	t.Helper()
	e, st := testEngine(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"ramp/gpt-5.6-luna","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + body + `}]}],"usage":{"input_tokens":900,"output_tokens":20}}`))
	}))
	t.Cleanup(server.Close)
	cfg := config.Default()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.Providers = map[string]config.ProviderConfig{"ramp": {APIKey: "test", BaseURL: server.URL}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	return e, st, &calls
}

// judgeSession seeds a session with assistant tool calls, since the judge needs
// recent activity to rule on and fails cheap without it.
func judgeSession(t *testing.T, e *Engine, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "find the top bar", Phase: "explore", BudgetUSD: 5}); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"Checkpoint", "Compact", "topbar"} {
		msg := provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: pattern, Name: "search", Arguments: map[string]any{"pattern": pattern}}}}
		if err := st.AddMessage(ctx, sid, "assistant", msg); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

func TestAllowInterventionDeclinedRaisesFloor(t *testing.T) {
	e, st, calls := judgeEngine(t, `"{\"intervene\":false,\"reason\":\"searching new terms\"}"`, 200)
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	p.noProgressTurns = 4

	if e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 4, p, nil) {
		t.Fatal("a declined verdict must suppress the intervention")
	}
	if p.floors["no_progress_turns"] != 6 {
		t.Fatalf("floor = %d, want 6", p.floors["no_progress_turns"])
	}
	// The verdict is memoised: the same value must not re-ask the judge.
	before := calls.Load()
	if e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 5, p, nil) {
		t.Fatal("value below the floor must not intervene")
	}
	if calls.Load() != before {
		t.Fatalf("judge re-asked below the floor: %d -> %d", before, calls.Load())
	}
	// Once the signal clears the floor the question re-opens and the judge is
	// asked again, backing off further on another decline.
	e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 6, p, nil)
	if calls.Load() != before+1 {
		t.Fatalf("judge calls = %d, want one more than %d", calls.Load(), before)
	}
	if p.floors["no_progress_turns"] != 9 {
		t.Fatalf("floor = %d, want 9 after a second decline", p.floors["no_progress_turns"])
	}
}

func TestAllowInterventionApprovedActsAndLeavesFloors(t *testing.T) {
	e, st, _ := judgeEngine(t, `"{\"intervene\":true,\"reason\":\"repeating the same search\"}"`, 200)
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	if !e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 4, p, nil) {
		t.Fatal("an approved verdict must allow the intervention")
	}
	if len(p.floors) != 0 {
		t.Fatalf("an approved verdict must not back off: %v", p.floors)
	}
}

// An unreachable or incoherent judge must fail cheap in both directions: do not
// act, and do not desensitise the session for the rest of the phase.
func TestAllowInterventionFailsCheap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"provider error", `""`, 500},
		{"unparseable verdict", `"I think it is fine actually"`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, _ := judgeEngine(t, tc.body, tc.status)
			sid := judgeSession(t, e, st)
			p := newProgressTracker()
			p.beginTurn("explore")
			if e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 4, p, nil) {
				t.Fatal("must not intervene when the judge cannot answer")
			}
			if len(p.floors) != 0 {
				t.Fatalf("a failed judge must not raise floors: %v", p.floors)
			}
		})
	}
}

// With the judge disabled the cascade must vanish: a tripped counter acts on
// its own, exactly as before, and no provider call is made.
func TestJudgeDisabledPreservesOldBehaviour(t *testing.T) {
	e, st, calls := judgeEngine(t, `"{\"intervene\":false,\"reason\":\"x\"}"`, 200)
	cfg, registry, _, _, _ := e.runtimeSnapshot()
	disabled := false
	cfg.Interventions.JudgeEnabled = &disabled
	e.ReplaceRuntime(cfg, registry, nil)
	sid := judgeSession(t, e, st)
	p := newProgressTracker()
	p.beginTurn("explore")
	if !e.allowIntervention(context.Background(), sid, "exploration_worker", "no_progress_turns", 4, p, nil) {
		t.Fatal("a disabled judge must let the counter act")
	}
	if calls.Load() != 0 {
		t.Fatalf("a disabled judge must not call a provider, got %d calls", calls.Load())
	}
}

func TestJudgeFailsCheapWithoutActivity(t *testing.T) {
	e, st, calls := judgeEngine(t, `"{\"intervene\":true,\"reason\":\"x\"}"`, 200)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "nothing yet", Phase: "explore", BudgetUSD: 5}); err != nil {
		t.Fatal(err)
	}
	p := newProgressTracker()
	p.beginTurn("explore")
	if e.allowIntervention(ctx, sid, "exploration_worker", "no_progress_turns", 4, p, nil) {
		t.Fatal("no activity to judge must fail cheap")
	}
	if calls.Load() != 0 {
		t.Fatalf("must not spend a judge call with nothing to judge, got %d", calls.Load())
	}
}

func TestRecentActivityDigestCarriesToolCalls(t *testing.T) {
	e, st, _ := judgeEngine(t, `"{}"`, 200)
	sid := judgeSession(t, e, st)
	digest := e.recentActivityDigest(context.Background(), sid)
	for _, want := range []string{"search", "Checkpoint", "topbar"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q: %s", want, digest)
		}
	}
}
