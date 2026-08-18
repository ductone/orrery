package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

func TestSessionEventsCacheAndRouting(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvent(ctx, "s", "one", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvent(ctx, "s", "two", nil); err != nil {
		t.Fatal(err)
	}
	es, err := s.EventsAfter(ctx, "s", 1)
	if err != nil || len(es) != 1 || es[0].Seq != 2 {
		t.Fatalf("events %+v %v", es, err)
	}
	if err = s.WarmCache(ctx, "s", "m", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, err := s.Cache(ctx, "s", "m")
	if err != nil || !c.Valid(time.Now()) || c.WarmPrefixTokens != 42 {
		t.Fatalf("cache %+v %v", c, err)
	}
	r := RoutingRecord{ID: "r", SessionID: "s", Turn: 1, DecisionPoint: "turn", StateJSON: "{}", CandidatesJSON: "[]", ChosenModel: "m", ChosenEffort: "low", CacheEstJSON: "{}", Explanation: "because"}
	if err = s.WriteRouting(ctx, r); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err = s.ExportRouting(ctx, time.Unix(0, 0), func([]byte) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("export n=%d err=%v", n, err)
	}
}

func TestIntegratedSessionAndMessageIdempotency(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	x := Session{ID: "native", Spec: "task", BudgetUSD: 5, Integration: "squire", ExternalID: "task-1", ExternalIncarnation: "1", WorkspacePath: "/work", WorkspaceOwnership: "external"}
	created, fresh, err := s.CreateSessionAccepted(ctx, x, "create-1", "turn-1", "hash-1")
	if err != nil || !fresh || created.ID != "native" {
		t.Fatalf("create session: fresh=%v session=%+v err=%v", fresh, created, err)
	}
	retried, fresh, err := s.CreateSessionAccepted(ctx, Session{ID: "other", Spec: "different", BudgetUSD: 1, Integration: "squire", ExternalID: "task-1", ExternalIncarnation: "1"}, "create-1", "turn-x", "hash-1")
	if err != nil || fresh || retried.ID != "native" {
		t.Fatalf("retry session: fresh=%v session=%+v err=%v", fresh, retried, err)
	}
	receipt, err := s.AcceptMessage(ctx, "native", "msg-1", "turn-2", "squire", "message-hash", map[string]string{"content": "continue"}, nil)
	if err != nil || receipt.Duplicate {
		t.Fatalf("accept message: %+v err=%v", receipt, err)
	}
	receipt, err = s.AcceptMessage(ctx, "native", "msg-1", "ignored", "squire", "message-hash", map[string]string{"content": "continue"}, nil)
	if err != nil || !receipt.Duplicate || receipt.TurnID != "turn-2" {
		t.Fatalf("retry message: %+v err=%v", receipt, err)
	}
	if _, err = s.AcceptMessage(ctx, "native", "msg-1", "turn-3", "squire", "different", map[string]string{"content": "changed"}, nil); err == nil {
		t.Fatal("expected request ID payload conflict")
	}
	events, err := s.EventsAfter(ctx, "native", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	for i, event := range events {
		if event.SchemaVersion != 1 || event.EventID == "" || event.SessionID != "native" || event.Seq != i+1 {
			t.Fatalf("bad envelope at %d: %+v", i, event)
		}
	}
	messages, err := s.Messages(ctx, "native")
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

func TestMigrationAddsIntegrationColumns(t *testing.T) {
	path := t.TempDir() + "/db.sqlite"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, spec TEXT NOT NULL, durable_summary TEXT NOT NULL DEFAULT '', phase TEXT NOT NULL DEFAULT 'plan', model TEXT NOT NULL DEFAULT '', turn INTEGER NOT NULL DEFAULT 0, spent_usd REAL NOT NULL DEFAULT 0, budget_usd REAL NOT NULL, status TEXT NOT NULL DEFAULT 'running', created_at TEXT NOT NULL, updated_at TEXT NOT NULL); CREATE TABLE events(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL, data_json TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(session_id,seq));`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(context.Background(), Session{ID: "s", Spec: "task", BudgetUSD: 1, Integration: "squire", ExternalID: "t"}); err != nil {
		t.Fatal(err)
	}
	x, err := s.Session(context.Background(), "s")
	if err != nil || x.Integration != "squire" || x.ExternalID != "t" {
		t.Fatalf("session=%+v err=%v", x, err)
	}
}

func TestInterruptedAndDeleteLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AddEvent(ctx, "s", "event", nil)
	_ = s.AddMessage(ctx, "s", "user", map[string]string{"content": "hello"})
	if err = s.MarkRunningInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	x, err := s.Session(ctx, "s")
	if err != nil || x.Status != "interrupted" {
		t.Fatalf("session=%+v err=%v", x, err)
	}
	if err = s.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, "s"); err != sql.ErrNoRows {
		t.Fatalf("expected deleted session, got %v", err)
	}
}
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.AddEvent(ctx, "s", "event", nil); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.EventsAfter(ctx, "s", 0)
	if err != nil || len(events) != 20 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestCheckpointRestoreAndFork(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", DurableSummary: "before", Phase: "implement", BudgetUSD: 5}); err != nil {
		t.Fatal(err)
	}
	_ = s.AddMessage(ctx, "s", "user", map[string]string{"content": "original"})
	_ = s.SetTodos(ctx, "s", []Todo{{Text: "first", Phase: "implement", Status: "in_progress"}})
	cp, err := s.CreateCheckpoint(ctx, "cp", "s", "safe point", "manual")
	if err != nil {
		t.Fatal(err)
	}
	x, _ := s.Session(ctx, "s")
	x.DurableSummary = "after"
	_ = s.UpdateSession(ctx, x)
	_ = s.AddMessage(ctx, "s", "assistant", map[string]string{"content": "later"})
	if err = s.RestoreCheckpoint(ctx, "s", cp.ID); err != nil {
		t.Fatal(err)
	}
	x, _ = s.Session(ctx, "s")
	messages, _ := s.Messages(ctx, "s")
	if x.DurableSummary != "before" || x.Status != "interrupted" || len(messages) != 1 {
		t.Fatalf("restored=%+v messages=%+v", x, messages)
	}
	fork, err := s.ForkSession(ctx, "s", "fork")
	if err != nil {
		t.Fatal(err)
	}
	forkMessages, _ := s.Messages(ctx, "fork")
	if fork.ID != "fork" || fork.SpentUSD != 0 || len(forkMessages) != 1 || fork.WorkspacePath != x.WorkspacePath {
		t.Fatalf("fork=%+v messages=%+v", fork, forkMessages)
	}
}

func TestPendingInputValidation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePendingInput(ctx, PendingInput{ID: "i", SessionID: "s", Question: "Choose", Choices: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolvePendingInput(ctx, "s", "c"); err == nil {
		t.Fatal("accepted invalid choice")
	}
	x, err := s.ResolvePendingInput(ctx, "s", "b")
	if err != nil || x.Answer != "b" {
		t.Fatalf("input=%+v err=%v", x, err)
	}
	if _, err = s.PendingInput(ctx, "s"); err != sql.ErrNoRows {
		t.Fatalf("pending err=%v", err)
	}
}

func TestQueuedMessageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	dup, err := s.EnqueueMessage(ctx, "s", "req-1", "follow-up task", "web", "hash-1", nil)
	if err != nil || dup {
		t.Fatalf("enqueue dup=%v err=%v", dup, err)
	}
	// Same request_id + payload is an idempotent no-op.
	dup, err = s.EnqueueMessage(ctx, "s", "req-1", "follow-up task", "web", "hash-1", nil)
	if err != nil || !dup {
		t.Fatalf("re-enqueue dup=%v err=%v", dup, err)
	}
	// Reused request_id with different content is rejected.
	if _, err = s.EnqueueMessage(ctx, "s", "req-1", "different", "web", "hash-2", nil); err == nil {
		t.Fatal("expected payload mismatch error")
	}
	q, ok, err := s.DequeueMessage(ctx, "s")
	if err != nil || !ok || q.Content != "follow-up task" || q.RequestID != "req-1" {
		t.Fatalf("dequeue q=%+v ok=%v err=%v", q, ok, err)
	}
	if _, ok, err = s.DequeueMessage(ctx, "s"); err != nil || ok {
		t.Fatalf("empty dequeue ok=%v err=%v", ok, err)
	}
}

// ApplyCompaction must land the durable summary, the message trim, and the
// cache invalidation together, and must be a no-op for an unknown session.
func TestApplyCompactionAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", DurableSummary: "before", Phase: "implement", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err = s.AddMessage(ctx, "s", "assistant", map[string]string{"content": "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.WarmCache(ctx, "s", "m", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	sess.DurableSummary = "after"
	if err = s.ApplyCompaction(ctx, sess, 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.DurableSummary != "after" {
		t.Fatalf("durable summary not applied: %+v", got)
	}
	msgs, err := s.Messages(ctx, "s")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages not trimmed: n=%d err=%v", len(msgs), err)
	}
	c, err := s.Cache(ctx, "s", "m")
	if err != nil || c.Valid(time.Now()) {
		t.Fatalf("cache not invalidated: %+v err=%v", c, err)
	}

	// An unknown session must be a no-op, not a partial write.
	if err = s.ApplyCompaction(ctx, Session{ID: "missing", DurableSummary: "x"}, 1); err != nil {
		t.Fatalf("unknown session should not error: %v", err)
	}
	got, err = s.Session(ctx, "s")
	if err != nil || got.DurableSummary != "after" {
		t.Fatalf("unknown-session compaction corrupted state: %+v err=%v", got, err)
	}
}

// A checkpoint must snapshot pending ask state and restore it, so restoring a
// checkpoint does not silently drop an unanswered user question.
func TestCheckpointRestoresPendingInput(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePendingInput(ctx, PendingInput{ID: "i", SessionID: "s", Question: "Choose", Choices: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	cp, err := s.CreateCheckpoint(ctx, "cp", "s", "safe point", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the ask, then restore the checkpoint taken while it was pending.
	if _, err = s.ResolvePendingInput(ctx, "s", "b"); err != nil {
		t.Fatal(err)
	}
	if err = s.RestoreCheckpoint(ctx, "s", cp.ID); err != nil {
		t.Fatal(err)
	}
	x, err := s.PendingInput(ctx, "s")
	if err != nil || x.ID != "i" || x.Status != "pending" {
		t.Fatalf("pending input not restored: %+v err=%v", x, err)
	}
}

// SetTodos must populate the continuation ledger: an active work item when work
// is in progress, and a persistent report obligation once work completes, even
// if the todo plan is later cleared.
func TestSetTodosPopulatesContinuationLedger(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "pending"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.SessionID == "" || cont.ActiveWorkItemID == "" || !cont.FinalReportRequired {
		t.Fatalf("ledger not populated: %+v", cont)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("work items n=%d err=%v", len(items), err)
	}
	activeID := cont.ActiveWorkItemID
	var active WorkItem
	for _, wi := range items {
		if wi.ID == activeID {
			active = wi
		}
	}
	if active.Objective != "Run the pinned-plan UI verification" || active.Status != "in_progress" {
		t.Fatalf("active work item wrong: %+v", active)
	}

	// Complete everything: the report obligation must persist.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Implement the pinned-plan UI", Phase: "implement", Status: "completed"},
		{Text: "Run the pinned-plan UI verification", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	cont, err = s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.ActiveWorkItemID != "" || !cont.FinalReportRequired {
		t.Fatalf("completed ledger wrong: %+v", cont)
	}

	// Clearing the plan must not drop the report obligation.
	if err = s.SetTodos(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	cont, err = s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !cont.FinalReportRequired {
		t.Fatalf("report obligation lost after clearing plan: %+v", cont)
	}
	items, err = s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("completed work items must be retained: n=%d err=%v", len(items), err)
	}
}

// A newly completed work item must carry a completion timestamp, and deleting a
// session with ledger rows must not trip the foreign key.
func TestLedgerCompletedStampAndDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Ship the UI", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 1 {
		t.Fatalf("work items n=%d err=%v", len(items), err)
	}
	if items[0].CompletedAt.IsZero() {
		t.Fatalf("newly completed work item missing completed_at: %+v", items[0])
	}
	if err = s.DeleteSession(ctx, "s"); err != nil {
		t.Fatalf("delete session with ledger failed: %v", err)
	}
}

// Restoring a checkpoint must rebuild the continuation ledger from the restored
// todos so the authoritative continuation state does not diverge.
func TestCheckpointRestoreResyncsLedger(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Fix the retry race", Phase: "implement", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	cp, err := s.CreateCheckpoint(ctx, "cp", "s", "safe point", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Advance the plan, then restore the checkpoint taken while the first todo
	// was in progress.
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Fix the retry race", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.RestoreCheckpoint(ctx, "s", cp.ID); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.ActiveWorkItemID == "" || !cont.FinalReportRequired {
		t.Fatalf("ledger not resynced to restored todos: %+v", cont)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 1 || items[0].Status != "in_progress" {
		t.Fatalf("ledger work items not resynced: n=%d err=%v", len(items), err)
	}
}

// Duplicate todo objectives must map to separate work items, not collapse into
// one row with a ghost duplicate.
func TestSyncWorkItemsDuplicateObjectives(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "pending"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("duplicate objectives must yield 2 work items: n=%d err=%v", len(items), err)
	}
	if items[0].ID == items[1].ID {
		t.Fatal("duplicate objectives share a work item ID")
	}
}

// When a completed and a pending work item share an objective, shrinking the
// plan to the pending occurrence must not overwrite the retained completed row.
func TestSyncWorkItemsPreservesCompletedDuplicate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	// Shrink to just the pending occurrence.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("completed duplicate must be retained: n=%d err=%v", len(items), err)
	}
	completed, pending := 0, 0
	for _, wi := range items {
		switch wi.Status {
		case "completed":
			completed++
		case "pending":
			pending++
		}
	}
	if completed != 1 || pending != 1 {
		t.Fatalf("expected 1 completed + 1 pending, got completed=%d pending=%d", completed, pending)
	}
	// Clearing the plan must keep the report obligation from the retained row.
	if err = s.SetTodos(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !cont.FinalReportRequired {
		t.Fatalf("report obligation lost after clearing plan: %+v", cont)
	}
}

// A non-completed todo must not consume a retained completed row even when it
// appears first in the plan; the completed history and its timestamp survive.
func TestSyncWorkItemsOrderIndependentCompleted(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 1 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	completedID := before[0].ID
	completedAt := before[0].CompletedAt

	// Replacement plan lists the pending occurrence first, then the completed
	// one. The pending todo must create a new row, not consume the completed.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "pending"},
		{Text: "Run the tests", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 work items: n=%d err=%v", len(items), err)
	}
	var retained *WorkItem
	for i := range items {
		if items[i].ID == completedID {
			retained = &items[i]
		}
	}
	if retained == nil || retained.Status != "completed" {
		t.Fatalf("completed row not retained: %+v", items)
	}
	if !retained.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed_at reset: before=%v after=%v", completedAt, retained.CompletedAt)
	}
}

// Equal objectives in different phases must be matched separately so a phase
// change does not relabel a retained completion across phases.
func TestSyncWorkItemsMatchesByPhase(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "implement", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 2 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	var implementCompleted *WorkItem
	for i := range before {
		if before[i].Phase == "implement" && before[i].Status == "completed" {
			implementCompleted = &before[i]
		}
	}
	if implementCompleted == nil {
		t.Fatal("missing implement/completed work item")
	}
	implementCompletedAt := implementCompleted.CompletedAt

	// Swap the statuses across phases: implement becomes pending, review becomes
	// completed. The implement completion history must survive.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "implement", Status: "pending"},
		{Text: "Run the tests", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var retained *WorkItem
	for i := range items {
		if items[i].ID == implementCompleted.ID {
			retained = &items[i]
		}
	}
	if retained == nil || retained.Status != "completed" {
		t.Fatalf("implement completion lost: %+v", items)
	}
	if !retained.CompletedAt.Equal(implementCompletedAt) {
		t.Fatalf("implement completed_at reset: before=%v after=%v", implementCompletedAt, retained.CompletedAt)
	}
}

// A completed todo must promote the live (non-completed) duplicate, not consume
// a retained completed row, so the current work's completion is recorded.
func TestSyncWorkItemsPromotesLiveDuplicate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 2 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	var pendingID string
	for _, wi := range before {
		if wi.Status == "pending" {
			pendingID = wi.ID
		}
	}
	if pendingID == "" {
		t.Fatal("missing pending work item")
	}
	// Complete the plan: the pending row must be promoted, not discarded.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var promoted *WorkItem
	for i := range items {
		if items[i].ID == pendingID {
			promoted = &items[i]
		}
	}
	if promoted == nil || promoted.Status != "completed" || promoted.CompletedAt.IsZero() {
		t.Fatalf("live duplicate not promoted: %+v", items)
	}
}

// Empty todo objectives must never become authoritative continuation rows.
func TestSyncWorkItemsSkipsEmptyObjective(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "  ", Phase: "review", Status: "pending"},
		{Text: "Real work", Phase: "implement", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 1 {
		t.Fatalf("empty objective must be skipped: n=%d err=%v", len(items), err)
	}
	if items[0].Objective != "Real work" {
		t.Fatalf("wrong work item: %+v", items[0])
	}
}

// A blank pending todo must not become the active work item; the fallback must
// skip it and pick the first real pending objective.
func TestSyncWorkItemsSkipsBlankPendingForActive(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "  ", Phase: "review", Status: "pending"},
		{Text: "Real work", Phase: "implement", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.ActiveWorkItemID == "" {
		t.Fatalf("active work item must not be blank: %+v", cont)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var active *WorkItem
	for i := range items {
		if items[i].ID == cont.ActiveWorkItemID {
			active = &items[i]
		}
	}
	if active == nil || active.Objective != "Real work" {
		t.Fatalf("active work item wrong: %+v", cont)
	}
}

// Submitting the same plan twice must be idempotent: it must not grow the
// ledger or create spurious completed work items.
func TestSyncWorkItemsIdempotentResubmission(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	plan := []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}
	if err = s.SetTodos(ctx, "s", plan); err != nil {
		t.Fatal(err)
	}
	first, err := s.WorkItems(ctx, "s")
	if err != nil || len(first) != 2 {
		t.Fatalf("first submission n=%d err=%v", len(first), err)
	}
	firstIDs := map[string]bool{}
	for _, wi := range first {
		firstIDs[wi.ID] = true
	}
	// Re-submit the identical plan.
	if err = s.SetTodos(ctx, "s", plan); err != nil {
		t.Fatal(err)
	}
	second, err := s.WorkItems(ctx, "s")
	if err != nil || len(second) != 2 {
		t.Fatalf("re-submission grew ledger: n=%d err=%v", len(second), err)
	}
	for _, wi := range second {
		if !firstIDs[wi.ID] {
			t.Fatalf("re-submission created a new work item: %+v", wi)
		}
	}
	completed := 0
	for _, wi := range second {
		if wi.Status == "completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("re-submission created spurious completions: %d", completed)
	}
}

// A live duplicate must be promoted even when an unrelated retained todo moves
// into its old slot: promotion is by plan membership, not numeric position.
func TestSyncWorkItemsPromotesLiveWhenSlotReused(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
		{Text: "Ship the UI", Phase: "implement", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 3 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	var pendingID string
	for _, wi := range before {
		if wi.Status == "pending" {
			pendingID = wi.ID
		}
	}
	if pendingID == "" {
		t.Fatal("missing pending work item")
	}
	// Drop the pending X and move Y into its old slot.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Ship the UI", Phase: "implement", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var promoted *WorkItem
	for i := range items {
		if items[i].ID == pendingID {
			promoted = &items[i]
		}
	}
	if promoted == nil || promoted.Status != "completed" || promoted.CompletedAt.IsZero() {
		t.Fatalf("live duplicate not promoted when slot reused: %+v", items)
	}
}

// When a same-key non-completed duplicate precedes the completed todo, the
// completed todo must promote the remaining live row, not the retained
// completed one, regardless of plan order.
func TestSyncWorkItemsPromotesRemainingLiveAfterPending(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 3 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	var completedID string
	for _, wi := range before {
		if wi.Status == "completed" {
			completedID = wi.ID
		}
	}
	if completedID == "" {
		t.Fatal("missing completed work item")
	}
	// Replace with [pending, completed]: the leading pending consumes one live
	// row; the completed todo must promote the remaining live row.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "pending"},
		{Text: "Run the tests", Phase: "review", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	// The retained completed row must survive, and a live row must be promoted.
	completed := 0
	for _, wi := range items {
		if wi.Status == "completed" {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("expected 2 completed work items (retained + promoted): %+v", items)
	}
}

// A completed todo must not consume a live row that a remaining non-completed
// todo needs; it matches a completed row or inserts a new one, so the result is
// independent of duplicate order.
func TestSyncWorkItemsReservesLiveForPending(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkItems(ctx, "s")
	if err != nil || len(before) != 1 {
		t.Fatalf("setup work items n=%d err=%v", len(before), err)
	}
	liveID := before[0].ID

	// Plan [completed, pending]: the completed todo must not steal the live row.
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Run the tests", Phase: "review", Status: "completed"},
		{Text: "Run the tests", Phase: "review", Status: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 work items: n=%d err=%v", len(items), err)
	}
	var live *WorkItem
	for i := range items {
		if items[i].ID == liveID {
			live = &items[i]
		}
	}
	if live == nil || live.Status != "pending" {
		t.Fatalf("live row not reserved for pending todo: %+v", items)
	}
}

// Acknowledging a delivered report must clear the continuation obligation and
// replace the durable summary, while retaining completed work items.
func TestAcknowledgeReportClearsObligation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", DurableSummary: `{"current_objective":"Wrap up X","pending_report":"Report X"}`, BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Ship the UI", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !cont.FinalReportRequired {
		t.Fatalf("expected report obligation: %+v", cont)
	}
	if err = s.AcknowledgeReport(ctx, "s", `{"objective":"task"}`); err != nil {
		t.Fatal(err)
	}
	cont, err = s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.FinalReportRequired || cont.ActiveWorkItemID != "" {
		t.Fatalf("obligation not cleared: %+v", cont)
	}
	got, err := s.Session(ctx, "s")
	if err != nil || got.DurableSummary != `{"objective":"task"}` {
		t.Fatalf("durable summary not replaced: %+v err=%v", got, err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 1 {
		t.Fatalf("completed history must be retained: n=%d err=%v", len(items), err)
	}
}

// AcknowledgeReport must clear the continuation even when the durable summary
// is not DurableState JSON (e.g. a todo snapshot), preserving the summary.
func TestAcknowledgeReportNonJSONSummary(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", DurableSummary: "[TODO SNAPSHOT] not json", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Ship the UI", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeReport(ctx, "s", "[TODO SNAPSHOT] not json"); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.FinalReportRequired || cont.ActiveWorkItemID != "" {
		t.Fatalf("obligation not cleared for non-JSON summary: %+v", cont)
	}
}

// After a report is acknowledged, clearing the plan must not re-open the
// obligation from retained completed work items.
func TestAcknowledgeReportNotReopenedByTodoSync(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{{Text: "Ship the UI", Phase: "implement", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeReport(ctx, "s", `{"objective":"task"}`); err != nil {
		t.Fatal(err)
	}
	// Clearing the plan must not re-assert the acknowledged obligation.
	if err = s.SetTodos(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.FinalReportRequired {
		t.Fatalf("acknowledged obligation re-opened by todo sync: %+v", cont)
	}
}

// An idempotent resubmission of an already-acknowledged completed todo must not
// re-open the report obligation.
func TestAcknowledgeReportNotReopenedByIdempotentResubmit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	plan := []Todo{{Text: "Ship the UI", Phase: "implement", Status: "completed"}}
	if err = s.SetTodos(ctx, "s", plan); err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeReport(ctx, "s", `{"objective":"task"}`); err != nil {
		t.Fatal(err)
	}
	// Resubmit the identical completed plan.
	if err = s.SetTodos(ctx, "s", plan); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.FinalReportRequired {
		t.Fatalf("idempotent resubmission re-opened acknowledged obligation: %+v", cont)
	}
}

// A checkpoint must snapshot the continuation ledger so restore reproduces
// retained completed work items and their completion chronology.
func TestCheckpointRestoreLedgerSnapshot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateSession(ctx, Session{ID: "s", Spec: "task", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTodos(ctx, "s", []Todo{
		{Text: "Ship the UI", Phase: "implement", Status: "completed"},
		{Text: "Run the verification", Phase: "review", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	cp, err := s.CreateCheckpoint(ctx, "cp", "s", "safe point", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Clear the plan, then restore: the retained completed work item and the
	// report obligation must come back from the ledger snapshot.
	if err = s.SetTodos(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.RestoreCheckpoint(ctx, "s", cp.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.WorkItems(ctx, "s")
	if err != nil || len(items) != 2 {
		t.Fatalf("ledger snapshot not restored: n=%d err=%v", len(items), err)
	}
	cont, err := s.Continuation(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if cont.ActiveWorkItemID == "" || !cont.FinalReportRequired {
		t.Fatalf("ledger continuation not restored: %+v", cont)
	}
}
