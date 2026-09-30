package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestShadowObservationLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	obs := ShadowObservation{ID: "o1", SessionID: "s", TurnID: "t", Turn: 3, Site: "phase", QuestionVersion: "phase/v1", Questions: map[string]any{"q": 1}, State: map[string]any{"task": "secret source"}, Baseline: map[string]any{"declared": "explore"}}
	if err := s.CreateShadow(ctx, obs); err != nil {
		t.Fatal(err)
	}
	// Baseline and outcome may land before the answer.
	if err := s.SetShadowOutcome(ctx, "o1", map[string]any{"status": "pass"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteShadow(ctx, "o1", "jev-1.13.0", map[string]any{"phase": map[string]any{"choice": "plan"}}, map[string]int{"input_tokens": 9}, 120*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateShadow(ctx, ShadowObservation{ID: "o2", SessionID: "s", Site: "review", QuestionVersion: "review/v1", Questions: 1, State: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteShadow(ctx, "o2", "", nil, nil, time.Second, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}

	recs, err := s.ShadowRecords(ctx, time.Time{}, "phase", false)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records %+v %v", recs, err)
	}
	b, _ := json.Marshal(recs[0])
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["state"] != nil || got["state_chars"].(float64) == 0 {
		t.Fatalf("state must be withheld by default: %s", b)
	}
	if got["answers"].(map[string]any)["phase"].(map[string]any)["choice"] != "plan" || got["baseline"].(map[string]any)["declared"] != "explore" || got["outcome"].(map[string]any)["status"] != "pass" || got["latency_ms"].(float64) != 120 {
		t.Fatalf("record = %s", b)
	}
	all, _ := s.ShadowRecords(ctx, time.Time{}, "", true)
	if len(all) != 2 || all[0].State == "" || all[1].Error != "timeout" || all[1].Answers != "" {
		t.Fatalf("all = %+v", all)
	}
	if err := s.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ShadowRecords(ctx, time.Time{}, "", false); len(left) != 0 {
		t.Fatalf("deleting a session must delete its observations: %+v", left)
	}
}

func TestLatestChildSession(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, x := range []Session{{ID: "p", Spec: "root", BudgetUSD: 1}, {ID: "c1", Spec: "review diff", ParentSessionID: "p", BudgetUSD: 1}, {ID: "c2", Spec: "review diff", ParentSessionID: "p", BudgetUSD: 1}, {ID: "c3", Spec: "other", ParentSessionID: "p", BudgetUSD: 1}} {
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got, err := s.LatestChildSession(ctx, "p", "review diff")
	if err != nil || got.ID != "c2" {
		t.Fatalf("got %q %v", got.ID, err)
	}
}
