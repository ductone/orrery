package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// memoryUpdateMessages are the MEMORY UPDATE harness messages in sid.
func memoryUpdateMessages(t *testing.T, e *Engine, sid string) []string {
	t.Helper()
	msgs, err := e.store.Messages(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		var pm provider.Message
		if err := json.Unmarshal([]byte(m.ContentJSON), &pm); err != nil {
			continue
		}
		if pm.Harness && strings.HasPrefix(pm.Content, "MEMORY UPDATE") {
			out = append(out, pm.Content)
		}
	}
	return out
}

func countEvents(t *testing.T, e *Engine, sid, typ string) int {
	t.Helper()
	events, err := e.store.EventsAfter(context.Background(), sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// A session selects its memory once and keeps the rendered segment byte for
// byte across phases and memory mutations, even when the selector would
// choose differently on every call (a random exploration slot). Mutations
// reach the model as MEMORY UPDATE messages; only compaction re-selects.
func TestMemoryEpochFixedForSession(t *testing.T) {
	ctx := context.Background()
	mc := config.Default().Memory
	mc.ShowIDs = true
	e := memoryEngine(t, mc)
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	a := commitMemory(t, e, w, "Alpha convention", "active", 0.9)
	b := commitMemory(t, e, w, "Bravo convention", "active", 0.9)
	p1 := commitMemory(t, e, w, "Pending one", "pending", 0.6)
	p2 := commitMemory(t, e, w, "Pending two", "pending", 0.6)
	calls := 0
	e.memorySelector = func(_ context.Context, req MemorySelectRequest) ([]storage.MemoryRecord, error) {
		calls++
		explore := p1
		if calls%2 == 0 {
			explore = p2
		}
		return []storage.MemoryRecord{a, b, explore}, nil
	}
	ids := func() []string {
		ep, _ := e.pinnedMemory("s1")
		var out []string
		for _, r := range ep.records {
			out = append(out, r.ID)
		}
		return out
	}
	e.refreshMemory(ctx, "s1", root, "spec", "plan", memoryBoundarySessionStart, 0, nil)
	first, _ := e.pinnedMemory("s1")
	firstIDs := ids()
	if pl := lastRetrieved(t, e, "s1"); pl.Reason != "selected" || pl.Outcome != "ok" {
		t.Fatalf("first payload: %+v", pl)
	}

	// Same phase: nothing recorded. Phase changes: reused, same boundary.
	e.refreshMemory(ctx, "s1", root, "spec", "plan", memoryBoundarySessionStart, 1, nil)
	e.refreshMemory(ctx, "s1", root, "spec", "explore", memoryBoundaryPhaseTransition, 2, nil)
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryPhaseTransition, 3, nil)
	pl := lastRetrieved(t, e, "s1")
	if pl.Reason != "reused" || pl.Outcome != "reused" || pl.CacheBoundaryID != first.boundaryID || !slices.Equal(pl.IDs, firstIDs) || pl.Source != "selector" {
		t.Fatalf("reuse payload: %+v", pl)
	}
	if n := countEvents(t, e, "s1", "memory.retrieved"); n != 3 {
		t.Fatalf("memory.retrieved events = %d, want 3", n)
	}

	// Another session's new record is not announced.
	if _, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "Other session note", Provenance: "extracted", Confidence: 0.9, Status: "active", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryPhaseTransition, 4, nil)
	if got := memoryUpdateMessages(t, e, "s1"); len(got) != 0 {
		t.Fatalf("unexpected updates: %q", got)
	}

	// The session's own record, a correction and a forget are delivered as
	// one message, and the prefix does not change.
	own, err := e.store.CommitMemory(ctx, store.MemoryRecord{WorkspaceID: w.ID, Kind: "fact", Text: "Own note", Provenance: "observed", Confidence: 1, Status: "active", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := e.store.ApplyMemoryMutationEvent(ctx, "s2", "", "memory.updated", map[string]any{}, store.MemoryMutation{Operation: "correct", WorkspaceID: w.ID, OldID: a.ID, Record: store.MemoryRecord{WorkspaceID: w.ID, Scope: a.Scope, Kind: a.Kind, Text: "Alpha convention, corrected", Provenance: "user", Confidence: 1, Status: "active"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.ForgetMemory(ctx, w.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryPhaseTransition, 5, nil)
	got := memoryUpdateMessages(t, e, "s1")
	if len(got) != 1 {
		t.Fatalf("updates = %q, want 1", got)
	}
	msg := got[0]
	for _, want := range []string{
		"- new: [fact/agent id=" + own.ID + "] Own note",
		"- corrected: [fact/agent id=" + a.ID + "] Alpha convention\n  now: [fact/agent id=" + fixed.ID + "] Alpha convention, corrected",
		"- forgotten, disregard: [fact/agent id=" + b.ID + "] Bravo convention",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("update lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Other session note") {
		t.Fatalf("update announced another session's note:\n%s", msg)
	}
	if n := countEvents(t, e, "s1", "memory.update_delivered"); n != 1 {
		t.Fatalf("memory.update_delivered events = %d, want 1", n)
	}

	// A pinned record that changed in place is reported once.
	p := first.records[2]
	p.Text, p.Status = p.Text+" (confirmed)", "active"
	if err := e.store.UpdateMemory(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryPhaseTransition, 6, nil)
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryPhaseTransition, 7, nil)
	got = memoryUpdateMessages(t, e, "s1")
	if len(got) != 2 || !strings.Contains(got[1], "- changed: [fact/agent id="+p.ID+"] [unconfirmed] ") || !strings.Contains(got[1], "(confirmed)") {
		t.Fatalf("updates = %q", got)
	}

	// Through all of it the prefix and the selection held.
	if calls != 1 {
		t.Fatalf("selector calls = %d, want 1", calls)
	}
	if r := e.memoryForRequest(e.cfg, "s1"); r != first.rendered || r == "" {
		t.Fatalf("rendered memory changed:\n%s\nvs\n%s", r, first.rendered)
	}
	if !slices.Equal(ids(), firstIDs) {
		t.Fatalf("ids %v, want %v", ids(), firstIDs)
	}

	// Compaction clears the epoch, so the next boundary selects afresh.
	e.setCacheBoundary("s1", "s1:compaction")
	e.refreshMemory(ctx, "s1", root, "spec", "wrap-up", memoryBoundaryCompaction, 8, nil)
	if calls != 2 {
		t.Fatalf("selector calls after compaction = %d, want 2", calls)
	}
}

// Without a selector the default ranking is kept the same way.
func TestMemoryEpochFixedWithDefaultRanking(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.Default().Memory)
	root := t.TempDir()
	w, _ := e.ensureMemoryWorkspace(ctx, root)
	commitMemory(t, e, w, "Run tests before pushing", "active", 0.9)
	e.refreshMemory(ctx, "s1", root, "tests", "plan", memoryBoundarySessionStart, 0, nil)
	first, _ := e.pinnedMemory("s1")
	commitMemory(t, e, w, "Another tests note", "active", 1)
	e.invalidateMemoryWorkspace(w.ID)
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundaryPhaseTransition, 2, nil)
	if got := e.memoryForRequest(e.cfg, "s1"); got != first.rendered || got == "" {
		t.Fatalf("rendered changed: %q vs %q", got, first.rendered)
	}
	if pl := lastRetrieved(t, e, "s1"); pl.Reason != "reused" || pl.Source != "default" || pl.CacheBoundaryID != first.boundaryID {
		t.Fatalf("payload: %+v", pl)
	}
}
