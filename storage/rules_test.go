package storage

import (
	"testing"
	"time"
)

func TestRequestText(t *testing.T) {
	if text, ok := RequestText(map[string]any{"role": "user", "content": "fix it"}); text != "fix it" || !ok {
		t.Fatalf("person's message = %q, %v", text, ok)
	}
	if _, ok := RequestText(map[string]any{"content": "budget raised", "harness": true}); ok {
		t.Fatal("a harness note counted as a request")
	}
}

func TestMemoryExpired(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	for _, c := range []struct {
		x    MemoryRecord
		want bool
	}{
		{MemoryRecord{Status: "active", ExpiresAt: &past}, true},
		{MemoryRecord{Status: "pending", ExpiresAt: &now}, true},
		{MemoryRecord{Status: "active", ExpiresAt: &future}, false},
		{MemoryRecord{Status: "active"}, false},
		{MemoryRecord{Status: "deleted", ExpiresAt: &past}, false},
	} {
		if got := MemoryExpired(c.x, now); got != c.want {
			t.Errorf("MemoryExpired(%s, expires %v) = %v", c.x.Status, c.x.ExpiresAt, got)
		}
	}
}

// TestSyncWorkItemsIsPure checks the sync on plain values: identity survives
// reordering, dropped live items go, and completed ones stay.
func TestSyncWorkItemsIsPure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	first := SyncWorkItems("s", []Todo{
		{Text: "a", Phase: "act", Status: "in_progress"},
		{Text: "b", Phase: "act", Status: "pending"},
		{Text: "c", Phase: "act", Status: "completed"},
	}, nil, false, now)
	if len(first.Items) != 3 || first.ActiveWorkItemID != first.Items[0].ID || !first.FinalReportRequired {
		t.Fatalf("first sync = %+v", first)
	}
	if !first.Items[2].CompletedAt.Equal(now) {
		t.Fatal("new completed item has no completion time")
	}

	second := SyncWorkItems("s", []Todo{{Text: "b", Phase: "act", Status: "in_progress"}}, first.Items, true, now.Add(time.Hour))
	ids := map[string]string{}
	for _, wi := range second.Items {
		ids[wi.Objective] = wi.ID
	}
	if len(second.Items) != 2 || ids["b"] != first.Items[1].ID || ids["c"] != first.Items[2].ID || ids["a"] != "" {
		t.Fatalf("second sync = %+v; want b kept by identity, c retained, a dropped", second.Items)
	}
	if second.ActiveWorkItemID != first.Items[1].ID {
		t.Fatalf("active = %s, want b", second.ActiveWorkItemID)
	}
}
