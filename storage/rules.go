package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The functions in this file are rules every Store implementation applies
// the same way. They operate on plain values so a backend can call them
// inside its own transaction.

// ApplyMemoryDefaults fills in a memory record's optional fields: Scope
// "workspace", Status "active", EvidenceRefs "[]".
func ApplyMemoryDefaults(x *MemoryRecord) {
	if x.Scope == "" {
		x.Scope = "workspace"
	}
	if x.Status == "" {
		x.Status = "active"
	}
	if x.EvidenceRefs == "" {
		x.EvidenceRefs = "[]"
	}
}

// ValidateMemoryRecord checks the fields of a memory record that do not
// depend on other records. Implementations also check that the workspace
// exists and that SupersededBy names a record in the same workspace.
func ValidateMemoryRecord(x MemoryRecord) error {
	if x.WorkspaceID == "" {
		return errors.New("workspace id is required")
	}
	if x.Scope != "workspace" && x.Scope != "project" {
		return errors.New("memory scope must be workspace or project")
	}
	switch x.Status {
	case "pending", "active", "superseded", "expired":
	default:
		return errors.New("invalid memory status")
	}
	if x.Kind == "" {
		return errors.New("memory kind is required")
	}
	if x.Text == "" {
		return errors.New("memory text is required")
	}
	if x.Provenance == "" {
		return errors.New("memory provenance is required")
	}
	if !slices.Contains(MemoryKinds, x.Kind) {
		return fmt.Errorf("memory kind must be one of %s", strings.Join(MemoryKinds, ", "))
	}
	if !slices.Contains(MemoryProvenances, x.Provenance) {
		return fmt.Errorf("memory provenance must be one of %s", strings.Join(MemoryProvenances, ", "))
	}
	if x.Confidence < 0 || x.Confidence > 1 {
		return errors.New("memory confidence must be between 0 and 1")
	}
	var evidence any
	if err := json.Unmarshal([]byte(x.EvidenceRefs), &evidence); err != nil {
		return errors.New("memory evidence refs must be a JSON array")
	}
	if _, ok := evidence.([]any); !ok {
		return errors.New("memory evidence refs must be a JSON array")
	}
	if x.Status == "superseded" && x.SupersededBy == "" {
		return errors.New("superseded memory requires a replacement")
	}
	if x.Status != "superseded" && x.SupersededBy != "" {
		return errors.New("only superseded memory may name a replacement")
	}
	return nil
}

// MemoryExpired reports whether ListMemory should treat x as expired at now:
// it is active or pending and its ExpiresAt has passed.
func MemoryExpired(x MemoryRecord, now time.Time) bool {
	return (x.Status == "active" || x.Status == "pending") && x.ExpiresAt != nil && !x.ExpiresAt.After(now)
}

// RequestText returns the text of a message accepted for a session and
// whether it is a person's request, which AcceptMessage and
// DeliverQueuedMessage record as the session's LatestRequest. A message
// marked "harness": true (a budget or limit choice, for example) is not.
func RequestText(content any) (text string, isRequest bool) {
	b, err := json.Marshal(content)
	if err != nil {
		return "", false
	}
	var m struct {
		Content string `json:"content"`
		Harness bool   `json:"harness"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Content, !m.Harness
}

// WorkItemSync is the work-item ledger SyncWorkItems derives from a todo plan.
type WorkItemSync struct {
	// Items is the session's complete set of work items after the sync:
	// existing items carried forward (same ID and CreatedAt) or created for
	// the plan's todos, in plan order, followed by completed items that are
	// no longer in the plan. Existing items absent from Items are deleted.
	Items               []WorkItem
	ActiveWorkItemID    string
	FinalReportRequired bool
}

// workItemKey groups work items by normalized objective and phase so equal
// objectives in different phases are matched separately.
func workItemKey(objective, phase string) string {
	return strings.ToLower(strings.TrimSpace(objective)) + "\x00" + strings.ToLower(strings.TrimSpace(phase))
}

// SyncWorkItems reconciles a session's work-item ledger to a new todo plan,
// as SetTodos must. existing is the session's current work items in the
// order they were created; priorReportRequired is the continuation's current
// FinalReportRequired. Work items are matched by normalized objective and
// phase, so their IDs and timestamps survive reordering; completed items are
// kept even when they leave the plan, so the report obligation stays concrete.
func SyncWorkItems(sid string, todos []Todo, existing []WorkItem, priorReportRequired bool, now time.Time) WorkItemSync {
	planNonCompleted := map[string]int{}
	for _, t := range todos {
		if strings.TrimSpace(t.Text) != "" && t.Status != "completed" {
			planNonCompleted[workItemKey(t.Text, t.Phase)]++
		}
	}
	consumed := map[string]bool{}
	byPosition := map[int]string{}
	var items []WorkItem
	var activeID string
	anyCompleted := false
	for i, t := range todos {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		key := workItemKey(t.Text, t.Phase)
		idx := -1
		if t.Status == "completed" {
			// Promote a live row that is being removed from the plan (its work
			// was just completed) before matching a retained completed row. A
			// live row is being removed when there are more live rows with this
			// key than the plan has non-completed todos for it.
			liveCount := 0
			for j := range existing {
				c := &existing[j]
				if !consumed[c.ID] && workItemKey(c.Objective, c.Phase) == key && c.Status != "completed" {
					liveCount++
				}
			}
			if liveCount > planNonCompleted[key] {
				for j := range existing {
					c := &existing[j]
					if !consumed[c.ID] && workItemKey(c.Objective, c.Phase) == key && c.Status != "completed" {
						idx = j
						break
					}
				}
			}
			// Otherwise match a completed row at the same position (idempotent
			// re-submission), then any completed row, then a live row.
			if idx < 0 {
				for j := range existing {
					c := &existing[j]
					if consumed[c.ID] || workItemKey(c.Objective, c.Phase) != key || c.Status != "completed" || c.Position != i {
						continue
					}
					idx = j
					break
				}
			}
			if idx < 0 {
				for j := range existing {
					c := &existing[j]
					if consumed[c.ID] || workItemKey(c.Objective, c.Phase) != key || c.Status != "completed" {
						continue
					}
					idx = j
					break
				}
			}
			// A completed todo never consumes a live row here: live rows are
			// reserved for the plan's remaining non-completed todos. If no
			// completed row matches, a new completed row is created below.
		} else {
			// Non-completed todo: match a live row at the same position, then
			// any live row; never consume a completed row.
			for j := range existing {
				c := &existing[j]
				if consumed[c.ID] || workItemKey(c.Objective, c.Phase) != key || c.Status == "completed" || c.Position != i {
					continue
				}
				idx = j
				break
			}
			if idx < 0 {
				for j := range existing {
					c := &existing[j]
					if consumed[c.ID] || workItemKey(c.Objective, c.Phase) != key || c.Status == "completed" {
						continue
					}
					idx = j
					break
				}
			}
		}
		// A non-completed todo consumes one of the plan's non-completed slots
		// for this key, so a later completed todo compares against the
		// remaining count.
		if t.Status != "completed" {
			planNonCompleted[key]--
		}
		var wi WorkItem
		wasCompleted := false
		if idx >= 0 {
			wi = existing[idx]
			wasCompleted = wi.Status == "completed"
			consumed[wi.ID] = true
			completedAt := time.Time{}
			if t.Status == "completed" {
				completedAt = wi.CompletedAt
				if !wasCompleted {
					completedAt = now
				}
			}
			wi.SessionID, wi.Status, wi.Phase, wi.Position, wi.CompletedAt = sid, t.Status, t.Phase, i, completedAt
		} else {
			wi = WorkItem{ID: uuid.NewString(), SessionID: sid, Objective: strings.TrimSpace(t.Text), Status: t.Status, Phase: t.Phase, Position: i, CreatedAt: now}
			if t.Status == "completed" {
				wi.CompletedAt = now
			}
		}
		items = append(items, wi)
		byPosition[i] = wi.ID
		if t.Status == "in_progress" && activeID == "" {
			activeID = wi.ID
		}
		// A completed todo creates a fresh obligation only when it is a new
		// completion (the work item was not already completed) or the prior
		// obligation was still owed; an idempotent resubmission of an
		// acknowledged completion does not re-open it.
		if t.Status == "completed" && (!wasCompleted || priorReportRequired) {
			anyCompleted = true
		}
	}
	// Unconsumed work items are no longer in the plan; completed ones are
	// retained as report subjects and the rest are dropped. Retained rows only
	// keep the report obligation if it was still owed: an acknowledged report
	// stays cleared.
	for _, wi := range existing {
		if !consumed[wi.ID] && wi.Status == "completed" {
			wi.SessionID = sid
			items = append(items, wi)
			if priorReportRequired {
				anyCompleted = true
			}
		}
	}
	if activeID == "" {
		for i, t := range todos {
			if t.Status == "pending" && byPosition[i] != "" {
				activeID = byPosition[i]
				break
			}
		}
	}
	return WorkItemSync{Items: items, ActiveWorkItemID: activeID, FinalReportRequired: anyCompleted || activeID != ""}
}
