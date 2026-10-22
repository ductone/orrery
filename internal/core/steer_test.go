package core

import (
	"context"
	"testing"

	"github.com/ductone/orrery/internal/store"
	"github.com/google/uuid"
)

func TestSteerWaitsForATurnBoundary(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "x", BudgetUSD: 1})
	count := func() int { ms, _ := st.Messages(ctx, sid); return len(ms) }

	if e.Steer(sid, "also this") {
		t.Fatal("an idle session cannot be steered")
	}
	e.beginRun(sid)
	if !e.Steer(sid, "also this") || count() != 0 {
		t.Fatal("a running session takes the message, not mid-turn")
	}
	e.drainSteers(ctx, sid, nil)
	if count() != 1 {
		t.Fatal("the turn boundary delivers it")
	}
	ms, _ := st.Messages(ctx, sid)
	if ms[0].Role != "user" {
		t.Fatalf("role %q", ms[0].Role)
	}
	// Steered after the last boundary: the run never saw it.
	e.Steer(sid, "late")
	e.endRun(sid)
	if got := e.Unsteered(sid); len(got) != 1 || got[0] != "late" {
		t.Fatalf("unsteered %v", got)
	}
	if got := e.Unsteered(sid); len(got) != 0 {
		t.Fatalf("unsteered twice %v", got)
	}
}
