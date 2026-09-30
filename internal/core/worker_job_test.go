package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestJobToolWaitsForWorkerJobs(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	sid, other := uuid.NewString(), uuid.NewString()
	for _, id := range []string{sid, other} {
		if err := st.CreateSession(ctx, store.Session{ID: id, Spec: "task", BudgetUSD: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateJob(ctx, store.Job{ID: "w1", SessionID: sid, Spec: "explore", ResultSchemaJSON: "{}", BudgetJSON: "{}", WorkspaceJSON: "{}", HintsJSON: "{}", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if view, err := e.workerJob(ctx, sid, "w1", "logs"); err != nil || view.(map[string]any)["status"] != "running" {
		t.Fatalf("logs = %v %v", view, err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = st.FinishJob(context.Background(), "w1", "pass", map[string]any{"answer": "found it"}, map[string]any{})
	}()
	view, err := e.workerJob(ctx, sid, "w1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(view)
	if !strings.Contains(string(b), `"status":"pass"`) || !strings.Contains(string(b), "found it") {
		t.Fatalf("wait returned %s", b)
	}
	if _, err := e.workerJob(ctx, other, "w1", "wait"); err == nil || !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("another session's worker must not be visible: %v", err)
	}
	if _, err := e.workerJob(ctx, sid, "w1", "cancel"); err == nil || !strings.Contains(err.Error(), "own budget") {
		t.Fatalf("cancel = %v", err)
	}
	if _, err := e.workerJob(ctx, sid, "missing", "wait"); err == nil {
		t.Fatal("an unknown id is not found")
	}
}
