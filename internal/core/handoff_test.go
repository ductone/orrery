package core

import (
	"context"
	"testing"

	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func roles(ms []provider.Message) string {
	var b []byte
	for _, m := range ms {
		switch m.Role {
		case "assistant":
			b = append(b, 'A')
		case "tool":
			b = append(b, 'T')
		case "user":
			b = append(b, 'U')
		}
	}
	return string(b)
}

func TestToolResultsArePairedWithTheirCalls(t *testing.T) {
	call := func(ids ...string) provider.Message {
		m := provider.Message{Role: "assistant"}
		for _, id := range ids {
			m.ToolCalls = append(m.ToolCalls, provider.ToolCall{ID: id, Name: "job"})
		}
		return m
	}
	result := func(id string) provider.Message { return provider.Message{Role: "tool", ToolCallID: id, Content: "{}"} }
	handoff := provider.Message{Role: "user", Harness: true, Content: "Worker job completed"}

	// The broken session: a handoff landed between job wait and its result.
	got := sanitizeProviderMessages([]provider.Message{{Role: "user", Content: "task"}, call("x"), handoff, result("x"), call("y"), result("y")})
	if roles(got) != "UATUAT" || got[2].ToolCallID != "x" || got[3].Content != handoff.Content {
		t.Fatalf("got %s", roles(got))
	}
	// Several calls with a message between their results.
	got = sanitizeProviderMessages([]provider.Message{call("a", "b"), result("a"), handoff, result("b")})
	if roles(got) != "ATTU" {
		t.Fatalf("got %s", roles(got))
	}
	// Well-formed history is unchanged.
	ok := []provider.Message{{Role: "user"}, call("a"), result("a"), {Role: "assistant", Content: "done"}}
	if got := sanitizeProviderMessages(ok); roles(got) != "UATA" {
		t.Fatalf("got %s", roles(got))
	}
	// A result never moves across the next assistant turn; the orphaned call is dropped.
	got = sanitizeProviderMessages([]provider.Message{call("a"), {Role: "assistant", Content: "later"}, result("a")})
	if len(got[0].ToolCalls) != 0 || roles(got) != "AA" {
		t.Fatalf("got %s calls=%d", roles(got), len(got[0].ToolCalls))
	}
}

func TestHandoffsWaitForATurnBoundary(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid := uuid.NewString()
	_ = st.CreateSession(ctx, store.Session{ID: sid, Spec: "x", BudgetUSD: 1})
	msg := provider.Message{Role: "user", Harness: true, Content: "Worker job w1 completed"}
	count := func() int { ms, _ := st.Messages(ctx, sid); return len(ms) }

	e.beginRun(sid)
	e.deliverHandoff(ctx, sid, "w1", msg)
	if count() != 0 {
		t.Fatal("a handoff to a running session must not be written mid-turn")
	}
	e.drainHandoffs(ctx, sid)
	if count() != 1 {
		t.Fatal("the turn boundary delivers it")
	}
	// A worker whose result came back through job wait is not handed off again.
	e.deliverHandoff(ctx, sid, "w2", msg)
	e.markDelivered("w2")
	e.drainHandoffs(ctx, sid)
	if count() != 1 {
		t.Fatal("a delivered worker's handoff is redundant")
	}
	e.endRun(sid)
	// An idle session receives handoffs immediately.
	e.deliverHandoff(ctx, sid, "w3", msg)
	if count() != 2 {
		t.Fatal("an idle session receives the handoff at once")
	}
	// Nested runs keep the session running until the last ends.
	e.beginRun(sid)
	e.beginRun(sid)
	e.endRun(sid)
	e.deliverHandoff(ctx, sid, "w4", msg)
	if count() != 2 {
		t.Fatal("still running")
	}
	e.endRun(sid)
}
