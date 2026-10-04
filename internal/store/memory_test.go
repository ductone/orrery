package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"database/sql"
)

func TestMemoryLifecycleAndWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w1, err := s.EnsureWorkspace(ctx, "repo:a")
	if err != nil {
		t.Fatal(err)
	}
	w1Again, err := s.EnsureWorkspace(ctx, "repo:a")
	if err != nil || w1Again.ID != w1.ID {
		t.Fatalf("unstable workspace: %+v %v", w1Again, err)
	}
	w2, err := s.EnsureWorkspace(ctx, "repo:b")
	if err != nil {
		t.Fatal(err)
	}

	old, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w1.ID, Kind: "preference", Text: "use tables", Provenance: "user", Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w1.ID, Kind: "preference", Text: "use prose", Provenance: "user", Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w2.ID, Kind: "fact", Text: "other workspace", Provenance: "observed", Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeMemory(ctx, w1.ID, old.ID, other.ID); err == nil {
		t.Fatal("cross-workspace supersession succeeded")
	}
	if err := s.SupersedeMemory(ctx, w1.ID, old.ID, replacement.ID); err != nil {
		t.Fatal(err)
	}
	// Supersession is terminal: a superseded source cannot be superseded again.
	if err := s.SupersedeMemory(ctx, w1.ID, old.ID, replacement.ID); !errors.Is(err, ErrMemoryNotSupersedable) {
		t.Fatalf("repeat supersession: %v", err)
	}

	active, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w1.ID, Status: "active"})
	if err != nil || len(active) != 1 || active[0].ID != replacement.ID {
		t.Fatalf("active memory: %+v %v", active, err)
	}
	if err := s.ForgetMemory(ctx, w1.ID, replacement.ID); err != nil {
		t.Fatal(err)
	}
	visible, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w1.ID})
	if err != nil || len(visible) != 1 || visible[0].Status != "superseded" {
		t.Fatalf("visible memory: %+v %v", visible, err)
	}
	// Forgetting is irreversible: a tombstone is not retrievable even by explicit status.
	deleted, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w1.ID, Status: "deleted"})
	if err != nil || len(deleted) != 0 {
		t.Fatalf("tombstone retrievable: %+v %v", deleted, err)
	}
	all, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w1.ID, IncludeExpired: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range all {
		if rec.ID == replacement.ID {
			t.Fatalf("tombstone listed: %+v", all)
		}
	}
	if err := s.ForgetMemory(ctx, w1.ID, replacement.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second forget: %v", err)
	}
}

func TestMemoryExpiryAndMigrationReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/db.sqlite"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.EnsureWorkspace(ctx, "repo:expiry")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "stale", Provenance: "observed", Confidence: .5, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID}); err != nil || len(got) != 0 {
		t.Fatalf("expired visible: %+v %v", got, err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true}); err != nil || len(got) != 1 {
		t.Fatalf("expired missing: %+v %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w2, err := s.EnsureWorkspace(ctx, "repo:expiry")
	if err != nil || w2.ID != w.ID {
		t.Fatalf("reopened workspace: %+v %v", w2, err)
	}
}

func TestMemoryReviewRegressions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := s.EnsureWorkspace(ctx, "repo:regress")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.EnsureWorkspace(ctx, "repo:regress-other")
	if err != nil {
		t.Fatal(err)
	}

	// An already-expired record stays retrievable via an explicit expired status filter.
	past := time.Now().UTC().Add(-time.Minute)
	expiredRec, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "stale", Provenance: "observed", Confidence: .5, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID}); err != nil || len(got) != 0 {
		t.Fatalf("expired leaked into default list: %+v %v", got, err)
	}

	// A sub-second future expiry must not be dropped by a lexical timestamp comparison.
	soon := time.Now().UTC().Add(500 * time.Millisecond)
	if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "brief", Provenance: "observed", Confidence: .5, ExpiresAt: &soon}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID}); err != nil || len(got) != 1 {
		t.Fatalf("sub-second expiry visible: %+v %v", got, err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true}); err != nil || len(got) != 2 {
		t.Fatalf("include expired: %+v %v", got, err)
	}

	// An explicit expired status filter must stay satisfiable.
	expiredByStatus, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "retired", Provenance: "observed", Confidence: .5, Status: "expired", ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, Status: "expired"})
	if err != nil || len(got) != 1 || got[0].ID != expiredByStatus.ID {
		t.Fatalf("explicit expired filter: %+v %v", got, err)
	}
	if got, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID}); err != nil || len(got) != 1 {
		t.Fatalf("expired status leaked into default list: %+v %v", got, err)
	}
	if expiredRec.ExpiresAt == nil || expiredRec.ExpiresAt.Location() != time.UTC {
		t.Fatalf("commit did not normalize expiry to UTC: %+v", expiredRec.ExpiresAt)
	}

	// UpdateMemory must not revive a tombstone.
	tombstone, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "gone", Provenance: "observed", Confidence: .5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetMemory(ctx, w.ID, tombstone.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMemory(ctx, MemoryRecord{ID: tombstone.ID, WorkspaceID: w.ID, Kind: "fact", Text: "revived", Provenance: "observed", Confidence: .5}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tombstone revived: %v", err)
	}

	// UpdateMemory applies defaults and rejects cross-workspace superseded_by.
	live, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "live", Provenance: "observed", Confidence: .5})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: other.ID, Kind: "fact", Text: "foreign", Provenance: "observed", Confidence: .5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMemory(ctx, MemoryRecord{ID: live.ID, WorkspaceID: w.ID, Kind: "fact", Text: "live2", Provenance: "observed", Confidence: .5}); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	var updated *MemoryRecord
	for i := range listed {
		if listed[i].ID == live.ID {
			updated = &listed[i]
		}
	}
	if updated == nil || updated.Scope != "workspace" || updated.EvidenceRefs != "[]" {
		t.Fatalf("update defaults not applied: %+v", listed)
	}
	if err := s.UpdateMemory(ctx, MemoryRecord{ID: live.ID, WorkspaceID: w.ID, Kind: "fact", Text: "live3", Provenance: "observed", Confidence: .5, Status: "superseded", SupersededBy: foreign.ID}); err == nil {
		t.Fatal("cross-workspace superseded_by accepted")
	}

	// SupersedeMemory must reject a deleted replacement record.
	src, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "src", Provenance: "observed", Confidence: .5})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "dst", Provenance: "observed", Confidence: .5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetMemory(ctx, w.ID, dst.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeMemory(ctx, w.ID, src.ID, dst.ID); !errors.Is(err, ErrMemoryNotSupersedable) {
		t.Fatalf("deleted replacement accepted: %v", err)
	}

	// SupersedeMemory reports a missing record distinctly.
	if err := s.SupersedeMemory(ctx, w.ID, src.ID, "no-such-id"); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("missing replacement: %v", err)
	}

	// evidence_refs must be a genuine JSON array.
	for _, bad := range []string{`{}`, `"x"`, `null`, `3`, `[`, ``} {
		if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "bad refs", Provenance: "observed", Confidence: .5, EvidenceRefs: bad}); err == nil && bad != `` {
			t.Fatalf("accepted non-array evidence refs: %q", bad)
		}
	}
	if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "good refs", Provenance: "observed", Confidence: .5, EvidenceRefs: `["session:1/event:2"]`}); err != nil {
		t.Fatalf("rejected valid evidence refs: %v", err)
	}
}

func TestApplyMemoryMutationEventPreconditionsAndPersistedRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.EnsureWorkspace(ctx, "repo:mutation")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, Session{ID: "s1", Spec: "mutation test", BudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	base := MemoryRecord{WorkspaceID: w.ID, Scope: "workspace", Kind: "fact", Text: "note", Provenance: "observed", Confidence: 1, Status: "active", EvidenceRefs: `["session:1/event:2"]`}

	// update with no id must fail the explicit precondition, not ErrNoRows.
	missingID := base
	missingID.ID = ""
	_, err = s.ApplyMemoryMutationEvent(ctx, "s1", "", "memory.updated", map[string]any{}, MemoryMutation{Operation: "update", Record: missingID})
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update without an id error = %v, want an explicit precondition error", err)
	}

	// update must enforce workspace existence like UpdateMemory does.
	badWorkspace := base
	badWorkspace.ID, badWorkspace.WorkspaceID = "m1", "no-such-workspace"
	if _, err := s.ApplyMemoryMutationEvent(ctx, "s1", "", "memory.updated", map[string]any{}, MemoryMutation{Operation: "update", Record: badWorkspace}); err == nil {
		t.Fatal("update accepted an unknown workspace")
	}

	committed, err := s.ApplyMemoryMutationEvent(ctx, "s1", "", "memory.committed", map[string]any{}, MemoryMutation{Operation: "commit", Record: base})
	if err != nil {
		t.Fatal(err)
	}
	if committed.ID == "" || committed.CreatedAt.IsZero() {
		t.Fatalf("commit must return the persisted row: %+v", committed)
	}

	// forget is addressed only by ID, and must still return the persisted row.
	forgotten, err := s.ApplyMemoryMutationEvent(ctx, "s1", "", "memory.forgotten", map[string]any{}, MemoryMutation{Operation: "forget", WorkspaceID: w.ID, MemoryID: committed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if forgotten.ID != committed.ID {
		t.Fatalf("forget returned %+v, want the mutated row %s", forgotten, committed.ID)
	}
	if forgotten.Status != "deleted" {
		t.Fatalf("forget returned status %q, want deleted", forgotten.Status)
	}
}
