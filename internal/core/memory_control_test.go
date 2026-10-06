package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
)

func memoryEngine(t *testing.T, cfg config.MemoryConfig) *Engine {
	t.Helper()
	e, _ := testEngine(t)
	e.cfg.Memory = cfg
	for _, sid := range []string{"s1", "s2"} {
		if err := e.store.CreateSession(context.Background(), store.Session{ID: sid, Spec: "memory test", BudgetUSD: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestMemoryControlLifecycle(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{RetainDays: 7})
	root := t.TempDir()
	var events []agentproto.AgentEvent
	emit := func(ev agentproto.AgentEvent) { events = append(events, ev) }

	proposed, err := e.controlMemory(ctx, "s1", root, "propose", map[string]any{
		"kind": "fact", "text": "tests live beside sources",
		"evidence_refs": []any{"internal/core/memory.go"},
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	rec := proposed.(store.MemoryRecord)
	if rec.Status != "pending" {
		t.Fatalf("proposal status = %q, want pending", rec.Status)
	}
	if rec.ExpiresAt == nil {
		t.Fatal("configured retention must set an expiry")
	}

	if _, err := e.controlMemory(ctx, "s1", root, "confirm", map[string]any{"id": rec.ID}, emit); err == nil {
		t.Fatal("confirm without explicit user confirmation must fail")
	}
	confirmed, err := e.controlMemory(ctx, "s1", root, "confirm", map[string]any{"id": rec.ID, "user_confirmed": true}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.(store.MemoryRecord).Status != "active" {
		t.Fatal("confirmed record must be active")
	}

	if _, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": rec.ID, "text": "tests live in a testdata directory"}, emit); err == nil {
		t.Fatal("correct without explicit user confirmation must fail")
	}
	corrected, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": rec.ID, "text": "tests live in a testdata directory", "user_confirmed": true}, emit)
	if err != nil {
		t.Fatal(err)
	}
	replacement := corrected.(store.MemoryRecord)
	if replacement.ID == rec.ID || replacement.Status != "active" {
		t.Fatalf("correction must create an active replacement: %+v", replacement)
	}

	active, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: replacement.WorkspaceID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != replacement.ID {
		t.Fatalf("corrected memory must leave only the replacement active: %+v", active)
	}

	if _, err := e.controlMemory(ctx, "s1", root, "forget", map[string]any{"id": replacement.ID}, emit); err == nil {
		t.Fatal("forget without explicit user confirmation must fail")
	}
	if _, err := e.controlMemory(ctx, "s1", root, "forget", map[string]any{"id": replacement.ID, "user_confirmed": true}, emit); err != nil {
		t.Fatal(err)
	}
	remaining, err := e.controlMemory(ctx, "s1", root, "inspect", map[string]any{}, emit)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range remaining.([]store.MemoryRecord) {
		if r.ID == replacement.ID {
			t.Fatal("forgotten record must not remain listed")
		}
	}

	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.Type] = true
		data, ok := ev.Data.(map[string]any)
		if !ok {
			t.Fatalf("event %s data has type %T", ev.Type, ev.Data)
		}
		if data["version"] == nil {
			t.Fatalf("event %s is missing a version", ev.Type)
		}
		if _, ok := data["text"]; ok {
			t.Fatalf("event %s leaked memory text", ev.Type)
		}
		if _, ok := data["workspace_id"]; ok {
			t.Fatalf("event %s leaked workspace identity", ev.Type)
		}
	}
	for _, typ := range []string{"memory.committed", "memory.updated", "memory.forgotten"} {
		if !seen[typ] {
			t.Fatalf("missing lifecycle event %s", typ)
		}
	}
}

func TestMemoryCorrectionGuardsAndPinnedCopy(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{MaxRecordBytes: 8})
	root := t.TempDir()
	pending, err := e.controlMemory(ctx, "s1", root, "propose", map[string]any{
		"kind": "fact", "text": "short", "evidence_refs": []any{"test"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := pending.(store.MemoryRecord)
	if _, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": rec.ID, "text": "changed", "user_confirmed": true}, nil); err == nil {
		t.Fatal("pending memory must not be corrected")
	}
	afterPending, err := e.controlMemory(ctx, "s1", root, "inspect", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pendingRecords := afterPending.([]store.MemoryRecord)
	if len(pendingRecords) != 1 || pendingRecords[0].ID != rec.ID || pendingRecords[0].Status != "pending" || pendingRecords[0].Text != "short" {
		t.Fatalf("rejected pending correction changed persisted memory: %+v", pendingRecords)
	}
	if _, err := e.controlMemory(ctx, "s1", root, "confirm", map[string]any{"id": rec.ID, "user_confirmed": true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": rec.ID, "text": strings.Repeat("x", 9), "user_confirmed": true}, nil); err == nil {
		t.Fatal("oversized correction must be rejected")
	}
	afterOversized, err := e.controlMemory(ctx, "s1", root, "inspect", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	activeRecords := afterOversized.([]store.MemoryRecord)
	if len(activeRecords) != 1 || activeRecords[0].ID != rec.ID || activeRecords[0].Status != "active" || activeRecords[0].Text != "short" {
		t.Fatalf("rejected oversized correction changed persisted memory: %+v", activeRecords)
	}

	e.memoryMu.Lock()
	e.memoryEpochs["s1"] = &memoryEpoch{records: []store.MemoryRecord{{ID: "original"}}}
	e.memoryMu.Unlock()
	snapshot, ok := e.pinnedMemory("s1")
	if !ok {
		t.Fatal("pinned memory missing")
	}
	snapshot.records[0].ID = "mutated"
	stored, _ := e.pinnedMemory("s1")
	if stored.records[0].ID != "original" {
		t.Fatal("pinned memory snapshot shares its records backing array")
	}
}

func TestMemoryControlInvalidatesPinnedMemory(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	w, ok := e.ensureMemoryWorkspace(ctx, root)
	if !ok {
		t.Fatal("memory workspace unavailable")
	}
	e.memoryMu.Lock()
	if e.memoryEpochs == nil {
		e.memoryEpochs = map[string]*memoryEpoch{}
	}
	e.memoryEpochs["s1"] = &memoryEpoch{workspaceID: w.ID, rendered: "pinned block", phase: "implement"}
	e.memoryEpochs["s2"] = &memoryEpoch{workspaceID: w.ID, rendered: "pinned block", phase: "implement"}
	e.memoryMu.Unlock()

	if _, err := e.controlMemory(ctx, "s1", root, "propose", map[string]any{
		"kind": "preference", "text": "prefer table tests",
		"evidence_refs": []any{"internal/core"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	e.memoryMu.Lock()
	pinned := e.memoryEpochs["s1"]
	otherPinned := e.memoryEpochs["s2"]
	e.memoryMu.Unlock()
	if pinned == nil || otherPinned == nil {
		t.Fatal("mutation must not drop pinned epochs mid-turn")
	}
	if pinned.rendered != "pinned block" || otherPinned.rendered != "pinned block" {
		t.Fatal("the already-cached memory segment must survive for the current turn")
	}
	if !pinned.stale || !otherPinned.stale {
		t.Fatal("memory mutation must mark every pinned epoch for the workspace stale")
	}
	if got := e.memoryForRequest(config.Config{Memory: config.MemoryConfig{Inject: true}}, "s1"); got != "pinned block" {
		t.Fatalf("memoryForRequest after mutation = %q, want the pinned block", got)
	}
	// A stale epoch must be re-selected at the next refresh boundary even when
	// the phase has not changed.
	e.refreshMemory(ctx, "s1", root, "tests", "implement", memoryBoundaryPhaseTransition, 2, nil)
	e.memoryMu.Lock()
	refreshed := e.memoryEpochs["s1"]
	e.memoryMu.Unlock()
	if refreshed == nil || refreshed.stale {
		t.Fatal("next refresh boundary must re-pin a fresh epoch")
	}
}

func TestMemoryControlRejectsMissingAndNonStringArguments(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	refs := []any{"evidence"}
	for _, args := range []map[string]any{
		{"kind": "fact", "evidence_refs": refs},
		{"kind": "fact", "text": 42, "evidence_refs": refs},
		{"kind": 42, "text": "value", "evidence_refs": refs},
	} {
		if _, err := e.controlMemory(ctx, "s1", root, "propose", args, nil); err == nil {
			t.Fatalf("propose accepted invalid arguments: %#v", args)
		}
	}
	proposed, err := e.controlMemory(ctx, "s1", root, "propose", map[string]any{"kind": "fact", "text": "original", "evidence_refs": refs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := proposed.(store.MemoryRecord).ID
	for _, text := range []any{nil, 42} {
		args := map[string]any{"id": id, "user_confirmed": true}
		if text != nil {
			args["text"] = text
		}
		if _, err := e.controlMemory(ctx, "s1", root, "correct", args, nil); err == nil {
			t.Fatalf("correct accepted invalid text: %#v", text)
		}
	}
	records, err := e.controlMemory(ctx, "s1", root, "inspect", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := records.([]store.MemoryRecord); len(got) != 1 || got[0].ID != id || got[0].Text != "original" {
		t.Fatalf("invalid corrections changed memory: %+v", got)
	}
}

func TestMemoryWorkspaceFallbackMatchesRefreshAndControl(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	e.cfg.WorkspaceRoot = t.TempDir()
	e.refreshMemory(ctx, "s1", "", "fact", "implement", memoryBoundarySessionStart, 1, nil)
	before, ok := e.pinnedMemory("s1")
	if !ok {
		t.Fatal("refresh did not pin a memory epoch")
	}
	if _, err := e.controlMemory(ctx, "s1", "", "propose", map[string]any{"kind": "fact", "text": "fallback", "evidence_refs": []any{"evidence"}}, nil); err != nil {
		t.Fatal(err)
	}
	after, ok := e.pinnedMemory("s1")
	if !ok || after.workspaceID != before.workspaceID || !after.stale {
		t.Fatalf("control and refresh resolved different workspaces: before=%+v after=%+v", before, after)
	}
}

func TestMemoryControlNotExposedToWorkers(t *testing.T) {
	e := memoryEngine(t, config.MemoryConfig{})
	req := agentproto.TaskRequest{Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "shared-write"}}
	worker := e.toolRegistry("s-worker", "parent-job", req, "", &instructionDiscovery{}, nil)
	for _, d := range worker.Definitions() {
		if d.Name == "memory" {
			t.Fatal("worker sessions must not expose the memory control")
		}
	}
	parent := e.toolRegistry("s-parent", "", req, "", &instructionDiscovery{}, nil)
	for _, d := range parent.Definitions() {
		if d.Name == "memory" {
			return
		}
	}
	t.Fatal("parent session must expose the memory control")
}

func TestMemoryControlReadOnlySessionHasNoMutations(t *testing.T) {
	e := memoryEngine(t, config.MemoryConfig{})
	req := agentproto.TaskRequest{Workspace: agentproto.Workspace{Path: t.TempDir(), Mode: "read"}}
	registry := e.toolRegistry("s-read", "", req, "", &instructionDiscovery{}, nil)
	var def *provider.Tool
	for i, d := range registry.Definitions() {
		if d.Name == "memory" {
			def = &registry.Definitions()[i]
		}
	}
	if def == nil {
		t.Fatal("read-only session should still expose memory inspection")
	}
	props, _ := def.InputSchema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 1 || enum[0] != "inspect" {
		t.Fatalf("read-only memory actions = %v, want [inspect]", enum)
	}
	for _, mutating := range []string{"propose", "confirm", "correct", "forget"} {
		if _, err := registry.Call(context.Background(), "memory", map[string]any{"action": mutating, "kind": "convention", "text": "x", "evidence_refs": []any{"a"}}); err == nil {
			t.Fatalf("read-only session allowed %s", mutating)
		}
	}
}

func TestMemoryProposeRejectsMalformedEvidence(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	for name, refs := range map[string]any{
		"scalar":      "internal/core",
		"object":      map[string]any{"path": "internal/core"},
		"empty array": []any{},
		"missing":     nil,
		"blank entry": []any{" "},
		"non-string":  []any{42},
	} {
		args := map[string]any{"kind": "convention", "text": "prefer table tests"}
		if refs != nil {
			args["evidence_refs"] = refs
		}
		if _, err := e.controlMemory(ctx, "s1", root, "propose", args, nil); err == nil {
			t.Fatalf("propose accepted %s evidence_refs", name)
		}
	}
	records, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: mustMemoryWorkspace(t, e, root), IncludeExpired: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("rejected proposals must not persist: %+v", records)
	}
}

func TestMemoryControlCannotRevivedExpiredRecord(t *testing.T) {
	ctx := context.Background()
	e := memoryEngine(t, config.MemoryConfig{})
	root := t.TempDir()
	workspaceID := mustMemoryWorkspace(t, e, root)
	expired, err := e.store.CommitMemory(ctx, store.MemoryRecord{
		WorkspaceID: workspaceID, Scope: "workspace", Kind: "fact",
		Text: "stale note", Provenance: "observed", Confidence: 1,
		Status: "expired", EvidenceRefs: `["internal/core"]`,
	})
	if err != nil {
		t.Fatal(err)
	}

	// inspect must still surface stored-expired records.
	listed, err := e.controlMemory(ctx, "s1", root, "inspect", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range listed.([]store.MemoryRecord) {
		if r.ID == expired.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("inspect must surface records stored with status expired")
	}

	// ...but they must not be confirmable or correctable.
	if _, err := e.controlMemory(ctx, "s1", root, "confirm", map[string]any{"id": expired.ID, "user_confirmed": true}, nil); err == nil {
		t.Fatal("confirm must reject an expired record")
	}
	if _, err := e.controlMemory(ctx, "s1", root, "correct", map[string]any{"id": expired.ID, "text": "fresh", "user_confirmed": true}, nil); err == nil {
		t.Fatal("correct must reject an expired record")
	}
	after, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: workspaceID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("expired record must not be revived: %+v", after)
	}
}

func mustMemoryWorkspace(t *testing.T, e *Engine, root string) string {
	t.Helper()
	w, ok := e.ensureMemoryWorkspace(context.Background(), root)
	if !ok {
		t.Fatal("memory workspace unavailable")
	}
	return w.ID
}
