package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func outcomeEvents(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateSession(ctx, Session{ID: "a", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	dec := func(id string) any {
		return map[string]any{"decision": map[string]any{"model": map[string]any{"id": id}}}
	}
	edit := map[string]any{"call": map[string]any{"name": "edit"}, "result": map[string]any{}}
	write := map[string]any{"call": map[string]any{"name": "exec", "arguments": map[string]any{"command": "sed -i s/a/b/ f.go"}}, "result": map[string]any{"ok": true}}
	read := map[string]any{"call": map[string]any{"name": "exec", "arguments": map[string]any{"command": "go test ./... 2>&1"}}, "result": map[string]any{"ok": true}}
	events := []struct {
		kind string
		data any
	}{
		{"routing.decision", dec("r/a")},
		{"tool.finished", edit},
		{"tool.finished", edit},
		{"routing.decision", dec("r/b")},
		{"tool.finished", write},
		{"tool.finished", read},
		{"job.started", map[string]any{"review": true, "model": "r/rev"}},
		{"review.outcome", map[string]any{"pass": false}},
		{"routing.decision", dec("r/a")},
		{"tool.finished", edit},
		{"job.started", map[string]any{"review": true, "model": "r/rev"}},
		{"review.outcome", map[string]any{"pass": true}},
		{"assistant.message", map[string]any{"model": "r/a"}},
		{"completion.answer_check", map[string]any{"off_topic": true}},
		{"assistant.message", map[string]any{"model": "r/a"}},
		{"completion.answer_check", map[string]any{"off_topic": false}},
		{"session.terminal", map[string]any{"status": "pass"}},
	}
	for _, ev := range events {
		if _, err := s.AddEvent(ctx, "a", ev.kind, ev.data); err != nil {
			t.Fatal(err)
		}
	}
}

func checkOutcomes(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.ModelOutcomes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]ModelOutcome{}
	for _, o := range got {
		m[o.Route] = o
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	a, b, rev := m["r/a"], m["r/b"], m["r/rev"]
	// First review failed: r/a authored 2 of 3 calls, r/b 1 of 3. The second
	// review passed r/a's single edit, so r/a gets one pass.
	if !near(a.AuthoredFail, 2.0/3) || !near(a.AuthoredPass, 1) || !near(b.AuthoredFail, 1.0/3) || b.AuthoredPass != 0 {
		t.Fatalf("authored a=%+v b=%+v", a, b)
	}
	if rev.Reviews != 2 || rev.Overturned != 0 {
		t.Fatalf("reviewer=%+v", rev)
	}
	if a.AnswerRejected != 1 || a.RunPass != 1 {
		t.Fatalf("a=%+v", a)
	}
}

func TestModelOutcomesLiveAndBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	outcomeEvents(t, s)
	checkOutcomes(t, s)
	// Drop the projection; reopening must rebuild it from events once.
	if _, err := s.db.Exec(`DELETE FROM model_outcomes`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checkOutcomes(t, s)
		s.Close()
	}
}

func TestModelOutcomesOverturn(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(ctx, Session{ID: "a", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		kind string
		data any
	}{
		{"routing.decision", map[string]any{"decision": map[string]any{"model": map[string]any{"id": "r/a"}}}},
		{"tool.finished", map[string]any{"call": map[string]any{"name": "edit"}}},
		{"job.started", map[string]any{"review": true, "model": "r/rev"}},
		{"review.outcome", map[string]any{"pass": false}},
		{"review.outcome", map[string]any{"pass": true, "disputed": true}},
	} {
		if _, err := s.AddEvent(ctx, "a", ev.kind, ev.data); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.ModelOutcomes(ctx)
	m := map[string]ModelOutcome{}
	for _, o := range got {
		m[o.Route] = o
	}
	if m["r/rev"].Overturned != 1 || m["r/a"].AuthoredPass != 1 || m["r/a"].AuthoredFail != 0 {
		t.Fatalf("outcomes=%+v", got)
	}
}
