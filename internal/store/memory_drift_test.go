package store

import (
	"context"
	"testing"
	"time"
)

func TestMemoryPersistedExpirySweep(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/memory.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.EnsureWorkspace(ctx, "repo:expiry-sweep")
	if err != nil {
		t.Fatal(err)
	}
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for _, status := range []string{"active", "pending", "deleted"} {
		rec, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: status, Provenance: "observed", Status: "active", ExpiresAt: &past})
		if err != nil {
			t.Fatal(err)
		}
		if status == "deleted" {
			if err := s.ForgetMemory(ctx, w.ID, rec.ID); err != nil {
				t.Fatal(err)
			}
		} else if status == "pending" {
			rec.Status = status
			if err := s.UpdateMemory(ctx, rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "future", Provenance: "observed", ExpiresAt: &future}); err != nil {
		t.Fatal(err)
	}
	expired, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, Status: "expired"})
	if err != nil || len(expired) != 2 {
		t.Fatalf("expired: %+v %v", expired, err)
	}
	for _, rec := range expired {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM memory_records WHERE memory_id=?`, rec.ID).Scan(&status); err != nil || status != "expired" {
			t.Fatalf("persisted status %q: %v", status, err)
		}
	}
	active, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID, Status: "active"})
	if err != nil || len(active) != 1 || active[0].Text != "future" {
		t.Fatalf("active: %+v %v", active, err)
	}
	var deleted int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_records WHERE workspace_id=? AND status='deleted'`, w.ID).Scan(&deleted); err != nil || deleted != 1 {
		t.Fatalf("tombstone expiry changed: %d %v", deleted, err)
	}
}

func TestMemoryLegacyEnumerationMigration(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy.sqlite"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.EnsureWorkspace(ctx, "repo:legacy")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "legacy memory", Provenance: "observed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_records SET kind='repository_note',provenance='tool_result' WHERE memory_id=?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	records, err := s.ListMemory(ctx, MemoryFilter{WorkspaceID: w.ID})
	if err != nil || len(records) != 1 || records[0].Kind != "fact" || records[0].Provenance != "observed" || records[0].Text != rec.Text {
		t.Fatalf("migration: %+v %v", records, err)
	}
	if err := s.UpdateMemory(ctx, records[0]); err != nil {
		t.Fatalf("migrated record not mutable: %v", err)
	}
	for _, kind := range MemoryKinds {
		for _, provenance := range MemoryProvenances {
			if _, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: kind, Text: "supported", Provenance: provenance}); err != nil {
				t.Errorf("%s/%s: %v", kind, provenance, err)
			}
		}
	}
	for _, invalid := range []MemoryRecord{
		{WorkspaceID: w.ID, Kind: "unknown", Text: "bad kind", Provenance: "observed"},
		{WorkspaceID: w.ID, Kind: "fact", Text: "bad provenance", Provenance: "unknown"},
	} {
		if _, err := s.CommitMemory(ctx, invalid); err == nil {
			t.Errorf("accepted invalid category: %+v", invalid)
		}
	}
}

func TestMemorySightingsCountDistinctSessions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/sightings.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.EnsureWorkspace(ctx, "repo:sightings")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CommitMemory(ctx, MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "sighting", Provenance: "extracted"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"s1", "s2"} {
		if err := s.CreateSession(ctx, Session{ID: sid}); err != nil {
			t.Fatal(err)
		}
	}
	for i, sid := range []string{"s1", "s1", "s2"} {
		want := 1
		if i == 2 {
			want = 2
		}
		if sessions, err := s.ObserveMemory(ctx, rec.ID, sid); err != nil || sessions != want {
			t.Fatalf("sessions=%d want=%d err=%v", sessions, want, err)
		}
	}
	var sightings int
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(sightings) FROM memory_sightings WHERE memory_id=?`, rec.ID).Scan(&sightings); err != nil || sightings != 3 {
		t.Fatalf("sightings=%d err=%v", sightings, err)
	}
}
