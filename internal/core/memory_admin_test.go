package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

func TestAdminMemory(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{RetainDays: 7})
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	rec, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "deploys are on Tuesdays", Provenance: "observed", Confidence: 1, Status: "active", SessionID: "s1", EvidenceRefs: `["session:s1/event:1"]`})
	if err != nil {
		t.Fatal(err)
	}
	admin := func(id, action, text string) (store.MemoryRecord, error) {
		return e.AdminMemory(ctx, root, id, action, text, "person:a@example.com via mcp", "wrong day")
	}

	// correct supersedes with a new active record.
	repl, err := admin(rec.ID, "correct", "deploys are on Thursdays")
	if err != nil {
		t.Fatal(err)
	}
	if repl.ID == rec.ID || repl.Status != "active" || repl.Text != "deploys are on Thursdays" || repl.Provenance != "user" || repl.EvidenceRefs != rec.EvidenceRefs {
		t.Fatalf("replacement = %+v", repl)
	}
	if _, err := admin(rec.ID, "correct", "again"); err == nil {
		t.Fatal("correcting a superseded record succeeded")
	}

	// restore brings the superseded record back, eligible for recall.
	back, err := admin(rec.ID, "restore", "")
	if err != nil {
		t.Fatal(err)
	}
	if back.Status != "active" || back.SupersededBy != "" || back.Text != rec.Text || back.Kind != rec.Kind || back.EvidenceRefs != rec.EvidenceRefs || back.ExpiresAt == nil {
		t.Fatalf("restored = %+v", back)
	}
	recalled, err := e.recallMemory(ctx, w.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range recalled {
		found = found || x.ID == rec.ID
	}
	if !found {
		t.Fatalf("restored record not recalled: %+v", recalled)
	}
	if _, err := admin(rec.ID, "restore", ""); err == nil {
		t.Fatal("restoring an active record succeeded")
	}

	// forget, then restore from deleted.
	gone, err := admin(repl.ID, "forget", "")
	if err != nil || gone.Status != "deleted" {
		t.Fatalf("forget = %+v, %v", gone, err)
	}
	if _, err := admin(repl.ID, "forget", ""); err == nil {
		t.Fatal("forgetting twice succeeded")
	}
	if again, err := admin(repl.ID, "restore", ""); err != nil || again.Status != "active" {
		t.Fatalf("restore deleted = %+v, %v", again, err)
	}

	// refusals
	if _, err := admin(rec.ID, "correct", " "); err == nil {
		t.Fatal("correct without text succeeded")
	}
	if _, err := admin(rec.ID, "purge", ""); err == nil {
		t.Fatal("unknown action succeeded")
	}
	if _, err := e.AdminMemory(ctx, root, rec.ID, "forget", "", "person:a@example.com via mcp", ""); err == nil {
		t.Fatal("forget without reason succeeded")
	}
	if _, err := admin(rec.ID, "forget", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AdminMemory(ctx, t.TempDir(), rec.ID, "restore", "", "person:a@example.com via mcp", "x"); !errors.Is(err, storage.ErrMemoryNotFound) {
		t.Fatalf("restore from another workspace = %v, want ErrMemoryNotFound", err)
	}

	// Every change is an audit event in the record's session.
	events, err := e.store.EventsAfter(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d["actor"] != "person:a@example.com via mcp" || d["reason"] == nil || d["to_status"] == nil {
			t.Fatalf("event %s data = %v", ev.Type, d)
		}
	}
	want := []string{"memory.updated", "memory.updated", "memory.forgotten", "memory.updated", "memory.forgotten"}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
}
