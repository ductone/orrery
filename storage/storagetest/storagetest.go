// Package storagetest checks that a storage.Store implementation meets the
// contracts documented in package storage. An implementation's tests call
// Run with a constructor that returns a fresh, empty store:
//
//	func TestConformance(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Store { return newStore(t) })
//	}
package storagetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/storage"
)

// Run runs every conformance test against stores returned by newStore.
func Run(t *testing.T, newStore func(t *testing.T) storage.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s storage.Store)
	}{
		{"SessionLifecycle", testSessionLifecycle},
		{"SessionListAndSpend", testSessionListAndSpend},
		{"CreateSessionAccepted", testCreateSessionAccepted},
		{"ForkSession", testForkSession},
		{"Messages", testMessages},
		{"TodosAndWorkItems", testTodosAndWorkItems},
		{"ApplyCompaction", testApplyCompaction},
		{"Checkpoints", testCheckpoints},
		{"Events", testEvents},
		{"PendingInputs", testPendingInputs},
		{"QueuedMessages", testQueuedMessages},
		{"AcceptMessage", testAcceptMessage},
		{"Jobs", testJobs},
		{"Memory", testMemory},
		{"MemoryWatermarkAndSightings", testMemoryWatermarkAndSightings},
		{"Routing", testRouting},
		{"RouteStats", testRouteStats},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func notFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("%s: err = %v, want storage.ErrNotFound", what, err)
	}
}

func newSession(t *testing.T, s storage.Store, id string) storage.Session {
	t.Helper()
	must(t, s.CreateSession(context.Background(), storage.Session{ID: id, Spec: "task " + id, BudgetUSD: 10, WorkspacePath: "/w"}))
	got, err := s.Session(context.Background(), id)
	must(t, err)
	return got
}

func testSessionLifecycle(t *testing.T, s storage.Store) {
	ctx := context.Background()
	got := newSession(t, s, "s1")
	if got.Phase != "plan" || got.Status != "running" || got.WorkspaceOwnership != "orrery" ||
		got.IntegrationContextJSON != "{}" || got.RequestJSON != "{}" || got.Title != storage.SessionTitle("task s1") {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}

	got.DurableSummary, got.Phase, got.Model, got.Turn, got.Status = "summary", "act", "m", 3, "completed"
	must(t, s.UpdateSession(ctx, got))
	must(t, s.UpdateSessionTitle(ctx, "s1", "New title"))
	after, err := s.Session(ctx, "s1")
	must(t, err)
	if after.DurableSummary != "summary" || after.Phase != "act" || after.Model != "m" || after.Turn != 3 || after.Status != "completed" || after.Title != "New title" {
		t.Fatalf("update not applied: %+v", after)
	}
	must(t, s.SetSessionStatus(ctx, "s1", "interrupted"))
	if after, _ = s.Session(ctx, "s1"); after.Status != "interrupted" {
		t.Fatalf("status = %q", after.Status)
	}

	_, err = s.Session(ctx, "missing")
	notFound(t, "Session(missing)", err)
	notFound(t, "AddBudget(missing)", s.AddBudget(ctx, "missing", 1))

	must(t, s.DeleteSession(ctx, "s1"))
	_, err = s.Session(ctx, "s1")
	notFound(t, "Session after delete", err)
	notFound(t, "DeleteSession(missing)", s.DeleteSession(ctx, "s1"))
}

func testSessionListAndSpend(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "older")
	time.Sleep(2 * time.Millisecond)
	newSession(t, s, "newer")
	must(t, s.CreateSession(ctx, storage.Session{ID: "child", Spec: "c", BudgetUSD: 1, ParentSessionID: "newer"}))

	list, err := s.Sessions(ctx)
	must(t, err)
	var ids []string
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	if fmt.Sprint(ids) != "[newer older]" {
		t.Fatalf("Sessions = %v, want top-level sessions newest first", ids)
	}

	must(t, s.AddSpend(ctx, "newer", 1.5))
	must(t, s.AddSpend(ctx, "child", 0.25))
	must(t, s.AddBudget(ctx, "newer", 5))
	x, _ := s.Session(ctx, "newer")
	if x.SpentUSD != 1.5 || x.BudgetUSD != 15 {
		t.Fatalf("spend %v budget %v", x.SpentUSD, x.BudgetUSD)
	}
	total, err := s.SpentUSDWithChildren(ctx, "newer")
	must(t, err)
	if total != 1.75 {
		t.Fatalf("SpentUSDWithChildren = %v, want 1.75", total)
	}
}

func testCreateSessionAccepted(t *testing.T, s storage.Store) {
	ctx := context.Background()
	in := storage.Session{ID: "int-1", Spec: "from slack", BudgetUSD: 5, Integration: "slack", ExternalID: "thread-9"}
	got, created, err := s.CreateSessionAccepted(ctx, in, "req-1", "turn-1", "hash")
	must(t, err)
	if !created || got.ID != "int-1" || got.WorkspaceOwnership != "external" {
		t.Fatalf("first accept: created=%v %+v", created, got)
	}
	events, err := s.EventsAfter(ctx, "int-1", 0)
	must(t, err)
	if len(events) != 2 || events[0].Seq != 1 || events[0].Type != "session.created" || events[1].Seq != 2 || events[1].Type != "turn.accepted" {
		t.Fatalf("events = %+v", events)
	}
	receipt, err := s.RequestReceipt(ctx, "int-1", "req-1")
	must(t, err)
	if receipt.TurnID != "turn-1" || receipt.PayloadHash != "hash" {
		t.Fatalf("receipt = %+v", receipt)
	}

	in.ID = "int-2"
	again, created, err := s.CreateSessionAccepted(ctx, in, "req-2", "turn-2", "hash")
	must(t, err)
	if created || again.ID != "int-1" {
		t.Fatalf("repeat accept: created=%v id=%s, want existing int-1", created, again.ID)
	}
	byExt, err := s.SessionByExternalID(ctx, "slack", "thread-9", "")
	must(t, err)
	if byExt.ID != "int-1" {
		t.Fatalf("SessionByExternalID = %s", byExt.ID)
	}
	_, err = s.SessionByExternalID(ctx, "slack", "other", "")
	notFound(t, "SessionByExternalID(other)", err)
}

func testForkSession(t *testing.T, s storage.Store) {
	ctx := context.Background()
	src := newSession(t, s, "src")
	src.DurableSummary = "context"
	must(t, s.UpdateSession(ctx, src))
	must(t, s.AddMessage(ctx, "src", "user", "hello"))
	must(t, s.SetTodos(ctx, "src", []storage.Todo{{Text: "step", Phase: "act", Status: "pending"}}))
	_, err := s.AddEvent(ctx, "src", "note", map[string]any{"x": 1})
	must(t, err)
	must(t, s.AddSpend(ctx, "src", 2))

	fork, err := s.ForkSession(ctx, "src", "fork")
	must(t, err)
	if fork.ID != "fork" || fork.WorkspacePath != src.WorkspacePath || fork.DurableSummary != "context" || fork.SpentUSD != 0 {
		t.Fatalf("fork = %+v", fork)
	}
	msgs, _ := s.Messages(ctx, "fork")
	todos, _ := s.Todos(ctx, "fork")
	events, _ := s.EventsAfter(ctx, "fork", 0)
	if len(msgs) != 1 || len(todos) != 1 || len(events) != 1 || events[0].Type != "session.forked" {
		t.Fatalf("fork has %d messages, %d todos, %d events; want 1, 1, and only session.forked", len(msgs), len(todos), len(events))
	}
}

func messageText(t *testing.T, m storage.Message) string {
	t.Helper()
	var s string
	must(t, json.Unmarshal([]byte(m.ContentJSON), &s))
	return s
}

func testMessages(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	for _, c := range []string{"one", strings.Repeat("x", 50), "three"} {
		must(t, s.AddMessage(ctx, "s", "user", c))
	}
	msgs, err := s.Messages(ctx, "s")
	must(t, err)
	if len(msgs) != 3 || messageText(t, msgs[0]) != "one" || messageText(t, msgs[2]) != "three" || msgs[0].Role != "user" {
		t.Fatalf("Messages = %+v", msgs)
	}

	n, err := s.ShrinkMessages(ctx, "s", "user", 20, func(string) string { return `"short"` })
	must(t, err)
	msgs, _ = s.Messages(ctx, "s")
	if n != 1 || messageText(t, msgs[1]) != "short" {
		t.Fatalf("ShrinkMessages changed %d, second message %q", n, msgs[1].ContentJSON)
	}

	must(t, s.ReplaceMessages(ctx, "s", msgs[:1]))
	if msgs, _ = s.Messages(ctx, "s"); len(msgs) != 1 {
		t.Fatalf("after ReplaceMessages: %d messages", len(msgs))
	}
}

func testApplyCompaction(t *testing.T, s storage.Store) {
	ctx := context.Background()
	sess := newSession(t, s, "s")
	for i := range 5 {
		must(t, s.AddMessage(ctx, "s", "user", fmt.Sprint(i)))
	}
	must(t, s.WarmCache(ctx, "s", "m", 1000, time.Hour))
	sess.DurableSummary = "compacted"
	must(t, s.ApplyCompaction(ctx, sess, 2))

	msgs, _ := s.Messages(ctx, "s")
	if len(msgs) != 2 || messageText(t, msgs[0]) != "3" || messageText(t, msgs[1]) != "4" {
		t.Fatalf("kept %d messages starting %q; want the newest 2", len(msgs), msgs[0].ContentJSON)
	}
	after, _ := s.Session(ctx, "s")
	if after.DurableSummary != "compacted" {
		t.Fatalf("summary = %q", after.DurableSummary)
	}
	if c, _ := s.Cache(ctx, "s", "m"); c.WarmPrefixTokens != 0 {
		t.Fatalf("cache warmth survived compaction: %+v", c)
	}
}

func testCheckpoints(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	must(t, s.AddMessage(ctx, "s", "user", "before"))
	must(t, s.SetTodos(ctx, "s", []storage.Todo{{Text: "a", Phase: "act", Status: "pending"}}))
	cp, err := s.CreateCheckpoint(ctx, "cp1", "s", "label", "reason")
	must(t, err)
	if cp.ID != "cp1" || cp.SessionID != "s" || cp.Label != "label" {
		t.Fatalf("checkpoint = %+v", cp)
	}

	must(t, s.AddMessage(ctx, "s", "user", "after"))
	must(t, s.SetTodos(ctx, "s", nil))
	must(t, s.CreatePendingInput(ctx, storage.PendingInput{ID: "q", SessionID: "s", Question: "?"}))

	must(t, s.RestoreCheckpoint(ctx, "s", "cp1"))
	msgs, _ := s.Messages(ctx, "s")
	todos, _ := s.Todos(ctx, "s")
	pending, _ := s.PendingInputs(ctx, "s")
	sess, _ := s.Session(ctx, "s")
	if len(msgs) != 1 || messageText(t, msgs[0]) != "before" || len(todos) != 1 || len(pending) != 0 || sess.Status != "interrupted" {
		t.Fatalf("restore: %d messages, %d todos, %d pending inputs, status %q", len(msgs), len(todos), len(pending), sess.Status)
	}

	list, err := s.Checkpoints(ctx, "s")
	must(t, err)
	if len(list) != 1 || list[0].ID != "cp1" {
		t.Fatalf("Checkpoints = %+v", list)
	}
	_, err = s.Checkpoint(ctx, "missing")
	notFound(t, "Checkpoint(missing)", err)
	newSession(t, s, "other")
	if err := s.RestoreCheckpoint(ctx, "other", "cp1"); err == nil {
		t.Fatal("restored another session's checkpoint")
	}
}

func testEvents(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	for i := range 3 {
		ev, err := s.AddEvent(ctx, "s", "tick", map[string]int{"i": i})
		must(t, err)
		if ev.Seq != i+1 || ev.SessionID != "s" || ev.Type != "tick" {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
	ev, err := s.AddEventForTurn(ctx, "s", "turn-1", "tock", nil)
	must(t, err)
	if ev.Seq != 4 || ev.TurnID != "turn-1" {
		t.Fatalf("AddEventForTurn = %+v", ev)
	}
	after, err := s.EventsAfter(ctx, "s", 2)
	must(t, err)
	if len(after) != 2 || after[0].Seq != 3 || after[1].Seq != 4 {
		t.Fatalf("EventsAfter(2) = %+v", after)
	}
	var data map[string]int
	must(t, json.Unmarshal(after[0].Data, &data))
	if data["i"] != 2 {
		t.Fatalf("event data = %s", after[0].Data)
	}
}

func testPendingInputs(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	if err := s.CreatePendingInput(ctx, storage.PendingInput{ID: "bad", SessionID: "s"}); err == nil {
		t.Fatal("accepted a pending input without a question")
	}
	must(t, s.CreatePendingInput(ctx, storage.PendingInput{ID: "q1", SessionID: "s", Question: "first?"}))
	time.Sleep(2 * time.Millisecond)
	must(t, s.CreatePendingInput(ctx, storage.PendingInput{ID: "q2", SessionID: "s", Question: "pick", Choices: []string{"a", "b"}}))

	p, err := s.PendingInput(ctx, "s")
	must(t, err)
	if p.ID != "q2" || p.Status != "pending" || fmt.Sprint(p.Choices) != "[a b]" {
		t.Fatalf("PendingInput = %+v, want newest", p)
	}
	if _, err := s.ResolvePendingInput(ctx, "s", "c"); err == nil {
		t.Fatal("accepted an answer outside the choices")
	}
	answered, err := s.ResolvePendingInput(ctx, "s", "b")
	must(t, err)
	if answered.ID != "q2" || answered.Status != "answered" || answered.Answer != "b" {
		t.Fatalf("ResolvePendingInput = %+v", answered)
	}
	if p, _ = s.PendingInput(ctx, "s"); p.ID != "q1" {
		t.Fatalf("after answering q2, PendingInput = %q", p.ID)
	}
	all, err := s.PendingInputs(ctx, "s")
	must(t, err)
	if len(all) != 2 || all[0].ID != "q1" {
		t.Fatalf("PendingInputs = %+v, want both, oldest first", all)
	}
	_, err = s.PendingInput(ctx, "missing")
	notFound(t, "PendingInput(no session)", err)
}

func testQueuedMessages(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	ok, err := s.EnqueueMessage(ctx, "s", "r1", "first", "web", "h1", nil) // true means duplicate
	must(t, err)
	if ok {
		t.Fatal("first enqueue reported a duplicate")
	}
	if dup, err := s.EnqueueMessage(ctx, "s", "r1", "first", "web", "h1", nil); err != nil || !dup {
		t.Fatalf("retry: duplicate=%v err=%v, want a reported no-op", dup, err)
	}
	if _, err = s.EnqueueMessage(ctx, "s", "r1", "different", "web", "h2", nil); err == nil {
		t.Fatal("reused request id for different content")
	}
	_, err = s.EnqueueMessage(ctx, "s", "r2", "second", "web", "h2", nil)
	must(t, err)

	q, ok, err := s.DequeueMessage(ctx, "s")
	must(t, err)
	if !ok || q.RequestID != "r1" || q.Content != "first" {
		t.Fatalf("DequeueMessage = %+v %v", q, ok)
	}
	receipt, err := s.DeliverQueuedMessage(ctx, "s", q.RequestID, "turn-1", "message", q.PayloadHash, map[string]any{"role": "user", "content": q.Content}, nil)
	must(t, err)
	if receipt.RequestID != "r1" || receipt.TurnID != "turn-1" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if latest, _ := s.LatestRequest(ctx, "s"); latest != "first" {
		t.Fatalf("LatestRequest = %q", latest)
	}
	if q, ok, _ = s.DequeueMessage(ctx, "s"); !ok || q.RequestID != "r2" {
		t.Fatalf("second dequeue = %+v %v", q, ok)
	}
	if _, ok, _ = s.DequeueMessage(ctx, "s"); ok {
		t.Fatal("queue not empty")
	}
}

func testAcceptMessage(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	if latest, err := s.LatestRequest(ctx, "s"); err != nil || latest != "" {
		t.Fatalf("LatestRequest before any message = %q, %v", latest, err)
	}
	msg := map[string]any{"role": "user", "content": "do it"}
	first, err := s.AcceptMessage(ctx, "s", "r1", "turn-1", "message", "h1", msg, nil)
	must(t, err)
	if first.Duplicate {
		t.Fatal("first accept marked duplicate")
	}
	again, err := s.AcceptMessage(ctx, "s", "r1", "turn-2", "message", "h1", msg, nil)
	must(t, err)
	if !again.Duplicate || again.TurnID != "turn-1" {
		t.Fatalf("retry = %+v, want the original receipt marked duplicate", again)
	}
	if _, err := s.AcceptMessage(ctx, "s", "r1", "turn-3", "message", "h2", map[string]any{"role": "user", "content": "other"}, nil); err == nil {
		t.Fatal("reused request id for different content")
	}
	msgs, _ := s.Messages(ctx, "s")
	events, _ := s.EventsAfter(ctx, "s", 0)
	if len(msgs) != 1 || len(events) != 2 || events[0].Type != "turn.accepted" || events[1].Type != "user.message" {
		t.Fatalf("accept wrote %d messages and events %+v; want 1 message, turn.accepted and user.message", len(msgs), events)
	}
	if latest, _ := s.LatestRequest(ctx, "s"); latest != "do it" {
		t.Fatalf("LatestRequest = %q", latest)
	}
	_, err = s.AcceptMessage(ctx, "s", "r2", "turn-4", "message", "h3", map[string]any{"role": "user", "content": "budget note", "harness": true}, nil)
	must(t, err)
	if latest, _ := s.LatestRequest(ctx, "s"); latest != "do it" {
		t.Fatalf("a harness note replaced LatestRequest: %q", latest)
	}
	_, err = s.RequestReceipt(ctx, "s", "missing")
	notFound(t, "RequestReceipt(missing)", err)
}

func testJobs(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	for _, id := range []string{"j1", "j2"} {
		must(t, s.CreateJob(ctx, storage.Job{ID: id, SessionID: "s", Spec: "work", ResultSchemaJSON: "{}", BudgetJSON: `{"max_usd":1.5}`, WorkspaceJSON: "{}", HintsJSON: "{}", Model: "m", Status: "running", Depth: 1}))
	}
	reserved, err := s.ReservedJobUSD(ctx, "s")
	must(t, err)
	if reserved != 3 {
		t.Fatalf("ReservedJobUSD = %v, want 3", reserved)
	}
	must(t, s.FinishJob(ctx, "j1", "completed", map[string]string{"ok": "yes"}, nil))
	if reserved, _ = s.ReservedJobUSD(ctx, "s"); reserved != 1.5 {
		t.Fatalf("ReservedJobUSD after finish = %v, want 1.5", reserved)
	}
	j, err := s.Job(ctx, "j1")
	must(t, err)
	if j.Status != "completed" || !strings.Contains(j.ResultJSON, "yes") {
		t.Fatalf("Job = %+v", j)
	}
	jobs, err := s.Jobs(ctx, "s")
	must(t, err)
	if len(jobs) != 2 {
		t.Fatalf("Jobs = %d", len(jobs))
	}
	_, err = s.Job(ctx, "missing")
	notFound(t, "Job(missing)", err)
}

func testMemory(t *testing.T, s storage.Store) {
	ctx := context.Background()
	w, err := s.EnsureWorkspace(ctx, "repo-a")
	must(t, err)
	again, err := s.EnsureWorkspace(ctx, "repo-a")
	must(t, err)
	if again.ID != w.ID {
		t.Fatalf("EnsureWorkspace not stable: %s vs %s", w.ID, again.ID)
	}
	commit := func(text string) storage.MemoryRecord {
		t.Helper()
		rec, err := s.CommitMemory(ctx, storage.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: "fact", Text: text, Provenance: "observed", Confidence: 1})
		must(t, err)
		return rec
	}
	a, b := commit("a"), commit("b")
	if a.ID == "" || a.Status != "active" {
		t.Fatalf("CommitMemory = %+v", a)
	}
	list, err := s.ListMemory(ctx, storage.MemoryFilter{WorkspaceID: w.ID, Status: "active"})
	must(t, err)
	if len(list) != 2 {
		t.Fatalf("ListMemory = %d records", len(list))
	}

	must(t, s.SupersedeMemory(ctx, w.ID, a.ID, b.ID))
	must(t, s.ForgetMemory(ctx, w.ID, b.ID))
	if list, _ = s.ListMemory(ctx, storage.MemoryFilter{WorkspaceID: w.ID, Status: "active"}); len(list) != 0 {
		t.Fatalf("active after supersede and forget: %+v", list)
	}

	past := time.Now().Add(-time.Hour)
	_, err = s.CommitMemory(ctx, storage.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: "fact", Text: "stale", Provenance: "observed", Confidence: 1, ExpiresAt: &past})
	must(t, err)
	expired, err := s.ListMemory(ctx, storage.MemoryFilter{WorkspaceID: w.ID, Status: "expired"})
	must(t, err)
	if len(expired) != 1 || expired[0].Text != "stale" {
		t.Fatalf("expired = %+v", expired)
	}

	workspaces, err := s.MemoryWorkspaces(ctx)
	must(t, err)
	if len(workspaces) != 1 || workspaces[0].ID != w.ID {
		t.Fatalf("MemoryWorkspaces = %+v", workspaces)
	}

	newSession(t, s, "s")
	rec, err := s.ApplyMemoryMutationEvent(ctx, "s", "turn", "memory.committed", map[string]string{"k": "v"},
		storage.MemoryMutation{Operation: "commit", Record: storage.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: "fact", Text: "via event", Provenance: "observed", Confidence: 1}})
	must(t, err)
	events, _ := s.EventsAfter(ctx, "s", 0)
	if rec.Text != "via event" || len(events) != 1 || events[0].Type != "memory.committed" {
		t.Fatalf("ApplyMemoryMutationEvent: record %+v, %d events", rec, len(events))
	}
}

func testMemoryWatermarkAndSightings(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s1")
	newSession(t, s, "s2")
	if seq, err := s.MemoryWatermark(ctx, "s1"); err != nil || seq != 0 {
		t.Fatalf("initial watermark = %d, %v", seq, err)
	}
	must(t, s.AdvanceMemoryWatermark(ctx, "s1", 5))
	must(t, s.AdvanceMemoryWatermark(ctx, "s1", 3))
	if seq, _ := s.MemoryWatermark(ctx, "s1"); seq != 5 {
		t.Fatalf("watermark = %d, want 5 (never lowered)", seq)
	}

	w, err := s.EnsureWorkspace(ctx, "repo")
	must(t, err)
	rec, err := s.CommitMemory(ctx, storage.MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: "fact", Text: "t", Provenance: "extracted", Confidence: 0.5, Status: "pending"})
	must(t, err)
	if rec.Status != "pending" {
		t.Fatalf("explicit pending status not kept: %q", rec.Status)
	}
	for _, sid := range []string{"s1", "s1", "s2"} {
		_, err = s.ObserveMemory(ctx, rec.ID, sid)
		must(t, err)
	}
	if n, _ := s.ObserveMemory(ctx, rec.ID, "s2"); n != 2 {
		t.Fatalf("ObserveMemory = %d distinct sessions, want 2", n)
	}
}

func testRouting(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	c, err := s.Cache(ctx, "s", "m")
	must(t, err)
	if c.SessionID != "s" || c.Model != "m" || c.WarmPrefixTokens != 0 {
		t.Fatalf("empty Cache = %+v", c)
	}
	must(t, s.WarmCache(ctx, "s", "m", 1234, time.Minute))
	if c, _ = s.Cache(ctx, "s", "m"); c.WarmPrefixTokens != 1234 || c.TTL != time.Minute || c.LastHit.IsZero() {
		t.Fatalf("warm Cache = %+v", c)
	}

	must(t, s.WriteRouting(ctx, storage.RoutingRecord{ID: "r1", SessionID: "s", Turn: 1, DecisionPoint: "turn", StateJSON: "{}", CandidatesJSON: "[]", ChosenModel: "m", ChosenEffort: "low", CacheEstJSON: "{}"}))
	must(t, s.UpdateRoutingOutcome(ctx, "r1", "turn_outcome_json", map[string]bool{"ok": true}))
	must(t, s.UpdateLatestTurnRoutingOutcome(ctx, "s", 1, map[string]bool{"ok": true}))
	must(t, s.UpdateLatestJobRoutingOutcome(ctx, "s", map[string]bool{"ok": true}))
	must(t, s.UpdateSessionRoutingOutcome(ctx, "s", map[string]bool{"ok": true}))
}

func testRouteStats(t *testing.T, s storage.Store) {
	ctx := context.Background()
	must(t, s.RecordModelCall(ctx, "route-a", 2*time.Second, 100, false, 1000, 500))
	must(t, s.RecordModelFailure(ctx, "route-a", "provider_error"))
	stats, err := s.ModelStats(ctx)
	must(t, err)
	var found bool
	for _, st := range stats {
		if st.Route == "route-a" {
			found = true
			if st.Calls != 1 {
				t.Fatalf("Calls = %d, want 1", st.Calls)
			}
		}
	}
	if !found {
		t.Fatalf("ModelStats has no route-a: %+v", stats)
	}

	must(t, s.RecordModelOutcomes(ctx, []storage.ModelOutcome{{Route: "route-a", AuthoredFail: 0.5, Reviews: 1}, {Route: "route-b", RunPass: 1}}))
	must(t, s.RecordModelOutcomes(ctx, []storage.ModelOutcome{{Route: "route-a", AuthoredPass: 0.5, AuthoredFail: -0.5, Overturned: 1}}))
	outcomes, err := s.ModelOutcomes(ctx)
	must(t, err)
	if len(outcomes) != 2 || outcomes[0].Route != "route-a" || outcomes[1].Route != "route-b" {
		t.Fatalf("ModelOutcomes = %+v, want route-a and route-b in order", outcomes)
	}
	if a := outcomes[0]; a.AuthoredPass != 0.5 || a.AuthoredFail != 0 || a.Reviews != 1 || a.Overturned != 1 {
		t.Fatalf("route-a = %+v, want the deltas summed", a)
	}
}

func testTodosAndWorkItems(t *testing.T, s storage.Store) {
	ctx := context.Background()
	newSession(t, s, "s")
	if c, err := s.Continuation(ctx, "s"); err != nil || c.SessionID != "" || c.FinalReportRequired {
		t.Fatalf("Continuation before any todos = %+v, %v; want the zero value", c, err)
	}
	must(t, s.SetTodos(ctx, "s", []storage.Todo{
		{Text: "write it", Phase: "act", Status: "in_progress"},
		{Text: "test it", Phase: "verify", Status: "pending"},
	}))
	todos, err := s.Todos(ctx, "s")
	must(t, err)
	items, err := s.WorkItems(ctx, "s")
	must(t, err)
	c, err := s.Continuation(ctx, "s")
	must(t, err)
	if len(todos) != 2 || todos[1].Position != 1 || len(items) != 2 || c.ActiveWorkItemID != items[0].ID || !c.FinalReportRequired {
		t.Fatalf("after plan: todos %+v, items %+v, continuation %+v", todos, items, c)
	}
	first := items[0].ID

	// Reordering keeps work-item identity; completing and dropping keeps the
	// completed item as a report subject.
	must(t, s.SetTodos(ctx, "s", []storage.Todo{
		{Text: "test it", Phase: "verify", Status: "in_progress"},
		{Text: "write it", Phase: "act", Status: "completed"},
	}))
	must(t, s.SetTodos(ctx, "s", []storage.Todo{{Text: "test it", Phase: "verify", Status: "completed"}}))
	items, _ = s.WorkItems(ctx, "s")
	var kept bool
	for _, wi := range items {
		kept = kept || (wi.ID == first && wi.Status == "completed" && !wi.CompletedAt.IsZero())
	}
	if len(items) != 2 || !kept {
		t.Fatalf("work items = %+v, want both, with the first completed under its original ID", items)
	}

	must(t, s.AcknowledgeReport(ctx, "s", "reported"))
	c, _ = s.Continuation(ctx, "s")
	sess, _ := s.Session(ctx, "s")
	if c.FinalReportRequired || c.ActiveWorkItemID != "" || sess.DurableSummary != "reported" {
		t.Fatalf("after AcknowledgeReport: continuation %+v, summary %q", c, sess.DurableSummary)
	}
	// Resubmitting the acknowledged plan does not re-open the obligation.
	must(t, s.SetTodos(ctx, "s", []storage.Todo{{Text: "test it", Phase: "verify", Status: "completed"}}))
	if c, _ = s.Continuation(ctx, "s"); c.FinalReportRequired {
		t.Fatal("an acknowledged report was re-opened by an identical plan")
	}
}
