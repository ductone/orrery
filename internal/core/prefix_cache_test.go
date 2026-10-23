package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
)

// A todo change that moves the session's phase rewrites the durable
// summary's todo snapshot. The cached prefix (instructions and tools) must
// stay byte-identical across the session's calls anyway, and the snapshot
// must still reach the model, in the trailing request message.
func TestTodoSnapshotKeepsCachedPrefix(t *testing.T) {
	plan := func(items ...[3]string) map[string]any {
		var list []any
		for _, it := range items {
			list = append(list, map[string]any{"text": it[0], "phase": it[1], "status": it[2]})
		}
		return map[string]any{"items": list}
	}
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		switch n {
		case 1:
			return responsesCall("c1", "todo", plan([3]string{"look around", "explore", "in_progress"}, [3]string{"wrap it up", "wrap-up", "pending"}))
		case 2:
			return responsesCall("c2", "todo", plan([3]string{"look around", "explore", "completed"}, [3]string{"wrap it up", "wrap-up", "in_progress"}))
		case 3:
			return responsesCall("c3", "todo", plan([3]string{"look around", "explore", "completed"}, [3]string{"wrap it up", "wrap-up", "completed"}))
		default:
			return responsesText("all done")
		}
	}}
	r := runScripted(t, s, "shared-write")
	if r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	if len(s.requests) < 4 {
		t.Fatalf("requests = %d", len(s.requests))
	}
	prefix := func(n int) string {
		b, _ := json.Marshal(map[string]any{"instructions": s.requests[n]["instructions"], "tools": s.requests[n]["tools"]})
		return string(b)
	}
	for n := 1; n < len(s.requests); n++ {
		if prefix(n) != prefix(0) {
			t.Errorf("request %d changed the cached prefix", n)
		}
	}
	if strings.Contains(prefix(len(s.requests)-1), "TODO SNAPSHOT") {
		t.Error("todo snapshot in the cached prefix")
	}
	// The wrap-up snapshot (taken when the phase moved) travels last.
	input, _ := s.requests[2]["input"].([]any)
	if len(input) == 0 {
		t.Fatal("no input")
	}
	last, _ := json.Marshal(input[len(input)-1])
	if !strings.Contains(string(last), "TODO SNAPSHOT") || !strings.Contains(string(last), "wrap it up") || !strings.Contains(string(last), "CURRENT REQUEST") {
		t.Errorf("last input item lacks the todo snapshot: %s", last)
	}
}

func TestTodoSnapshotSurvivesCompaction(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	workspace := t.TempDir()
	req := agentproto.TaskRequest{Spec: "Fix the retry race", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 10_000, MaxWallClock: time.Second}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write"}}
	const sid = "compaction-snapshot"
	summary := setPlanSnapshot("", `[{"text":"snapshot step"}]`)
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: req.Spec, Phase: "implement", Status: "interrupted", BudgetUSD: 5, WorkspacePath: workspace, RequestJSON: store.JSON(req), DurableSummary: summary}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"Fix the retry race.", "Started.", "The fix is in retry.go.", "Running the tests.", "Tests pass.", "Preparing the report."} {
		role := "assistant"
		if i == 0 {
			role = "user"
		}
		if err := st.AddMessage(ctx, sid, role, provider.Message{Role: role, Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Compact(ctx, sid, "phase_or_context_boundary", nil); err != nil {
		t.Fatal(err)
	}
	s, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	rest, plan := splitPlanSnapshot(s.DurableSummary)
	if plan != `[{"text":"snapshot step"}]` {
		t.Fatalf("snapshot lost in compaction: %q", s.DurableSummary)
	}
	var state DurableState
	if err := json.Unmarshal([]byte(rest), &state); err != nil || strings.Contains(state.PriorSummary, "TODO SNAPSHOT") {
		t.Fatalf("durable state = %q (%v)", rest, err)
	}
}

func TestSplitPlanSnapshot(t *testing.T) {
	state := store.JSON(DurableState{Objective: "x", CurrentObjective: "now"})
	summary := setPlanSnapshot(state, `[{"text":"a"}]`)
	rest, plan := splitPlanSnapshot(summary)
	if rest != state || plan != `[{"text":"a"}]` {
		t.Fatalf("split = %q, %q", rest, plan)
	}
	// The durable spec stays the same JSON with or without a snapshot, and
	// the continuation anchors still come out of it.
	if got, want := durableSpec(store.Session{DurableSummary: summary}), durableSpec(store.Session{DurableSummary: state}); got != want || strings.Contains(got, `"current_objective":"now"`) {
		t.Fatalf("durable spec = %q, want %q", got, want)
	}
	if !strings.Contains(currentRequest(store.Session{DurableSummary: summary}, "q"), "CURRENT OBJECTIVE") {
		t.Fatal("current objective lost behind a snapshot")
	}
	if planSnapshotSection(state) != "" || !strings.Contains(planSnapshotSection(summary), `[{"text":"a"}]`) {
		t.Fatal("snapshot section")
	}
}
