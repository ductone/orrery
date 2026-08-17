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
