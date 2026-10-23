package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// AdminMemory changes one memory record of the workspace at workspacePath on
// a person's behalf, from outside any session: a host's operator console.
// action is one of
//
//   - "forget": the record becomes a deleted tombstone (as the memory tool's
//     forget), from any status but deleted.
//   - "restore": a superseded, deleted, expired or pending record becomes
//     active again with its text, kind and evidence, no superseded_by and a
//     fresh expiry. A replacement that superseded it stays as it is.
//   - "correct": an active record is superseded by a new active record with
//     text (as the memory tool's correct).
//
// The change and its audit event (memory.forgotten or memory.updated, with
// actor and reason in its data) commit together. The event goes to the
// session that created the record, so a record without one, or whose
// session is gone, cannot be changed here. It returns the record as stored
// after the change: for correct, the replacement. A record of another
// workspace is storage.ErrMemoryNotFound.
func (e *Engine) AdminMemory(ctx context.Context, workspacePath, id, action, text, actor, reason string) (store.MemoryRecord, error) {
	cfg, _, _, _, _ := e.runtimeSnapshot()
	if strings.TrimSpace(workspacePath) == "" {
		workspacePath = cfg.WorkspaceRoot
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return store.MemoryRecord{}, errors.New("actor and reason are required")
	}
	w, ok := e.ensureMemoryWorkspace(ctx, workspacePath)
	if !ok {
		return store.MemoryRecord{}, errors.New("memory store unavailable")
	}
	// ListMemory first stores elapsed expiries, so rec.Status is effective.
	records, err := e.store.ListMemory(ctx, store.MemoryFilter{WorkspaceID: w.ID, IncludeExpired: true, IncludeDeleted: true})
	if err != nil {
		return store.MemoryRecord{}, err
	}
	var rec store.MemoryRecord
	for _, x := range records {
		if x.ID == id {
			rec = x
		}
	}
	if rec.ID == "" {
		return store.MemoryRecord{}, fmt.Errorf("%w: %s", storage.ErrMemoryNotFound, id)
	}
	if rec.SessionID == "" {
		return store.MemoryRecord{}, errors.New("memory record has no session to record the change in")
	}
	var expiry *time.Time
	if days := cfg.Memory.ExpiresAfterDays(); days > 0 {
		t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
		expiry = &t
	}
	data := map[string]any{"id": rec.ID, "from_status": rec.Status, "actor": actor, "reason": reason, "version": memoryLifecycleEventVersion}
	var typ string
	var m store.MemoryMutation
	switch action {
	case "forget":
		if rec.Status == "deleted" {
			return store.MemoryRecord{}, errors.New("memory record is already deleted")
		}
		typ, data["to_status"] = "memory.forgotten", "deleted"
		m = store.MemoryMutation{Operation: "forget", WorkspaceID: w.ID, MemoryID: rec.ID}
	case "restore":
		if rec.Status == "active" {
			return store.MemoryRecord{}, errors.New("memory record is already active")
		}
		if rec.Scope != storage.ScopeAgent {
			return store.MemoryRecord{}, errors.New("only agent-scope memory can be modified here")
		}
		typ, data["to_status"] = "memory.updated", "active"
		m = store.MemoryMutation{Operation: "restore", WorkspaceID: w.ID, MemoryID: rec.ID, Record: store.MemoryRecord{ExpiresAt: expiry}}
	case "correct":
		if rec.Status != "active" {
			return store.MemoryRecord{}, errors.New("only active memory can be corrected")
		}
		if rec.Scope != storage.ScopeAgent {
			return store.MemoryRecord{}, errors.New("only agent-scope memory can be modified here")
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return store.MemoryRecord{}, errors.New("text is required to correct memory")
		}
		if len(text) > cfg.Memory.RecordBytes() {
			return store.MemoryRecord{}, fmt.Errorf("memory text exceeds max_record_bytes (%d)", cfg.Memory.RecordBytes())
		}
		typ, data["to_status"] = "memory.updated", "superseded"
		m = store.MemoryMutation{Operation: "correct", WorkspaceID: w.ID, OldID: rec.ID, Record: store.MemoryRecord{WorkspaceID: w.ID, Scope: rec.Scope, Kind: rec.Kind, Text: text, Provenance: "user", Confidence: 1, Status: "active", EvidenceRefs: rec.EvidenceRefs, ExpiresAt: expiry, SessionID: rec.SessionID, Origin: rec.Origin, Shadows: rec.Shadows}}
	default:
		return store.MemoryRecord{}, errors.New("action must be forget, restore or correct")
	}
	out, err := e.store.ApplyMemoryMutationEvent(ctx, rec.SessionID, "", typ, data, m)
	if err != nil {
		return store.MemoryRecord{}, err
	}
	e.invalidateMemoryWorkspace(w.ID)
	return out, nil
}
