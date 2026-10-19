package core

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/store"
)

const day = 24 * time.Hour

// retentionEngine returns an engine over a SQLite store whose clock the test
// controls: advance it to age every session.
func retentionEngine(t *testing.T, rc config.RetentionConfig) (*Engine, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := New(config.Config{Retention: rc}, st, nil, nil)
	clock := time.Now()
	e.now = func() time.Time { return clock }
	return e, st, &clock
}

func addSession(t *testing.T, st *store.Store, id, parent, status string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateSession(ctx, store.Session{ID: id, Spec: "task " + id, BudgetUSD: 1, ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(ctx, id, "user", "hello "+id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvent(ctx, id, "session.started", map[string]any{"spec": "task"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionStatus(ctx, id, status); err != nil {
		t.Fatal(err)
	}
}

// finishPassing records a passing terminal event, as a finished top-level run
// does, and optionally the extraction that follows it.
func finishPassing(t *testing.T, st *store.Store, id string, extracted bool) {
	t.Helper()
	ev, err := st.AddEvent(context.Background(), id, "session.terminal", map[string]any{"status": "pass"})
	if err != nil {
		t.Fatal(err)
	}
	if extracted {
		if err := st.AdvanceMemoryWatermark(context.Background(), id, ev.Seq); err != nil {
			t.Fatal(err)
		}
	}
}

func sessionState(t *testing.T, st *store.Store, id string) (exists, archived bool) {
	t.Helper()
	s, err := st.Session(context.Background(), id)
	if err != nil {
		return false, false
	}
	return true, !s.ArchivedAt.IsZero()
}

func TestMaintainStorageDeletesExpiredWorkers(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{})
	addSession(t, st, "top", "", "pass")
	addSession(t, st, "worker", "top", "pass")
	addSession(t, st, "busy", "top", "running")
	addSession(t, st, "orphan", "gone", "fail")

	rep, err := e.MaintainStorage(ctx)
	if err != nil || len(rep.Deleted) != 0 || len(rep.Archived) != 0 {
		t.Fatalf("fresh pass = %+v, %v", rep, err)
	}
	*clock = clock.Add(25 * time.Hour)
	rep, err = e.MaintainStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(rep.Deleted)
	if !slices.Equal(rep.Deleted, []string{"orphan", "worker"}) || len(rep.Archived) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if ok, _ := sessionState(t, st, "busy"); !ok {
		t.Fatal("running worker was deleted")
	}
	if ok, _ := sessionState(t, st, "top"); !ok {
		t.Fatal("top-level session was deleted")
	}
	if rep.BytesBefore <= 0 || rep.BytesAfter <= 0 {
		t.Fatalf("bytes not measured: %+v", rep)
	}
}

func TestMaintainStorageArchivesAfterMemoryCatchesUp(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{})
	addSession(t, st, "lagging", "", "pass")
	finishPassing(t, st, "lagging", false)
	addSession(t, st, "done", "", "pass")
	finishPassing(t, st, "done", true)
	addSession(t, st, "failed", "", "fail") // extraction never consumes it
	addSession(t, st, "asking", "", "input_required")
	finishPassing(t, st, "asking", true)
	addSession(t, st, "parent", "", "pass")
	finishPassing(t, st, "parent", true)
	addSession(t, st, "child", "parent", "running")

	*clock = clock.Add(29 * day)
	if rep, err := e.MaintainStorage(ctx); err != nil || len(rep.Archived) != 0 {
		t.Fatalf("before ArchiveAfter = %+v, %v", rep, err)
	}
	*clock = clock.Add(2 * day)
	rep, err := e.MaintainStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(rep.Archived)
	if !slices.Equal(rep.Archived, []string{"done", "failed"}) {
		t.Fatalf("archived = %v, want done and failed only", rep.Archived)
	}
	for id, want := range map[string]bool{"done": true, "failed": true, "lagging": false, "asking": false, "parent": false} {
		if _, got := sessionState(t, st, id); got != want {
			t.Errorf("%s archived = %v, want %v", id, got, want)
		}
	}
	if msgs, _ := st.Messages(ctx, "done"); len(msgs) != 0 {
		t.Fatal("archived session kept its messages")
	}

	// Once extraction catches up, the lagging session is archived.
	if err := st.AdvanceMemoryWatermark(ctx, "lagging", 3); err != nil {
		t.Fatal(err)
	}
	rep, err = e.MaintainStorage(ctx)
	if err != nil || !slices.Equal(rep.Archived, []string{"lagging"}) {
		t.Fatalf("after catch-up = %+v, %v", rep, err)
	}
}

func TestMaintainStorageProtectsActiveSessions(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{})
	addSession(t, st, "active", "", "pass")
	finishPassing(t, st, "active", true)
	e.beginRun("active")
	*clock = clock.Add(60 * day)
	if rep, err := e.MaintainStorage(ctx); err != nil || len(rep.Archived) != 0 {
		t.Fatalf("archived an active session: %+v, %v", rep, err)
	}
	e.endRun("active")
	if rep, err := e.MaintainStorage(ctx); err != nil || len(rep.Archived) != 1 {
		t.Fatalf("idle session not archived: %+v, %v", rep, err)
	}
}

func TestMaintainStorageDeleteAfter(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{DeleteAfter: 40 * day})
	// ArchivedAt comes from the store's own clock, i.e. the real now, so the
	// engine clock below counts from the start of the test.
	addSession(t, st, "top", "", "pass")
	finishPassing(t, st, "top", true)
	addSession(t, st, "kid", "top", "pass")

	*clock = clock.Add(31 * day)
	rep, err := e.MaintainStorage(ctx)
	if err != nil || !slices.Equal(rep.Archived, []string{"top"}) {
		t.Fatalf("archive pass = %+v, %v", rep, err)
	}
	// The worker expired in the same pass.
	if ok, _ := sessionState(t, st, "kid"); ok {
		t.Fatal("expired worker survived")
	}
	*clock = clock.Add(8 * day)
	if rep, _ = e.MaintainStorage(ctx); len(rep.Deleted) != 0 {
		t.Fatalf("deleted before DeleteAfter: %+v", rep)
	}
	*clock = clock.Add(2 * day)
	rep, err = e.MaintainStorage(ctx)
	if err != nil || !slices.Equal(rep.Deleted, []string{"top"}) {
		t.Fatalf("delete pass = %+v, %v", rep, err)
	}
	if ok, _ := sessionState(t, st, "top"); ok {
		t.Fatal("archived session survived DeleteAfter")
	}
}

func TestMaintainStorageDeletingParentDeletesChildren(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{DeleteAfter: day, WorkerAfter: 1000 * day})
	addSession(t, st, "top", "", "pass")
	finishPassing(t, st, "top", true)
	addSession(t, st, "kid", "top", "pass")
	*clock = clock.Add(31 * day)
	if _, err := e.MaintainStorage(ctx); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * day)
	rep, err := e.MaintainStorage(ctx)
	if err != nil || !slices.Equal(rep.Deleted, []string{"kid", "top"}) {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}

func TestMaintainStorageBudgetArchivesOldestFirst(t *testing.T) {
	ctx := context.Background()
	e, st, _ := retentionEngine(t, config.RetentionConfig{})
	big := strings.Repeat("0123456789abcdef", 16*1024) // 256 KiB, compresses well
	for _, id := range []string{"a", "b", "c"} {
		addSession(t, st, id, "", "pass")
		if err := st.AddMessage(ctx, id, "assistant", big); err != nil {
			t.Fatal(err)
		}
		finishPassing(t, st, id, true)
		time.Sleep(2 * time.Millisecond)
	}
	addSession(t, st, "asking", "", "input_required")
	if err := st.AddMessage(ctx, "asking", "assistant", big); err != nil {
		t.Fatal(err)
	}
	used, err := st.UsageBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Room for the sessions minus about one and a half of them.
	e.cfg.Retention = config.RetentionConfig{MaxBytes: used - 384*1024, HighWaterFraction: 1}
	rep, err := e.MaintainStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Archived, []string{"a", "b"}) || len(rep.Deleted) != 0 || rep.OverBudget {
		t.Fatalf("report = %+v (used %d)", rep, used)
	}
	if _, archived := sessionState(t, st, "c"); archived {
		t.Fatal("newest session was archived though the budget was met")
	}
	if rep.BytesAfter >= rep.BytesBefore {
		t.Fatalf("usage did not fall: %+v", rep)
	}

	// An impossible budget deletes archives oldest first but never touches
	// the protected session, and reports being over budget.
	e.cfg.Retention = config.RetentionConfig{MaxBytes: 1, HighWaterFraction: 1}
	rep, err = e.MaintainStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Archived, []string{"c"}) || !slices.Equal(rep.Deleted, []string{"a", "b", "c"}) || !rep.OverBudget {
		t.Fatalf("over-budget report = %+v", rep)
	}
	if ok, _ := sessionState(t, st, "asking"); !ok {
		t.Fatal("protected session was deleted")
	}
}

func TestMaintainStorageWithoutArchiver(t *testing.T) {
	ctx := context.Background()
	e, st, clock := retentionEngine(t, config.RetentionConfig{})
	e.store = &wrappedStore{Store: st} // hides Archiver
	addSession(t, st, "top", "", "pass")
	finishPassing(t, st, "top", true)
	addSession(t, st, "kid", "top", "pass")
	*clock = clock.Add(60 * day)
	rep, err := e.MaintainStorage(ctx)
	if err != nil || !slices.Equal(rep.Deleted, []string{"kid"}) || len(rep.Archived) != 0 || rep.BytesBefore != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}
