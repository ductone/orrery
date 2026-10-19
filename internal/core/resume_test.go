package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
)

func waitForStatus(t *testing.T, st *store.Store, id, want string) store.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := st.Session(context.Background(), id); err == nil && s.Status == want {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := st.Session(context.Background(), id)
	t.Fatalf("session %s status = %q, want %q", id, s.Status, want)
	return s
}

// startFinished runs a task to completion and returns its session ID.
func startFinished(t *testing.T, e *Engine, workspace, spec string) string {
	t.Helper()
	req := agentproto.TaskRequest{Spec: spec, Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	sid, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-results
	return sid
}

// Only a session whose event log ends mid-run is resumed on wake. A crashed
// run continues with a harness note and an unchanged latest request; idle
// placeholders, finished sessions and restored sessions are left alone.
func TestResumeInterrupted(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	workspace := t.TempDir()
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("finished") }}
	srv := s.serve(t)
	cfg := gateConfig(workspace, srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .2}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)

	// Crashed mid-run: a later turn was accepted but never reached a terminal.
	crashed := startFinished(t, e, workspace, "original task")
	if _, err := st.AddEvent(ctx, crashed, "turn.accepted", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(ctx, crashed, "user", provider.Message{Role: "user", Content: "please keep going on the second part"}); err != nil {
		t.Fatal(err)
	}
	// Finished, then marked interrupted by some other path.
	finished := startFinished(t, e, workspace, "finished task")
	// Rewound by the person to a checkpoint.
	restored := startFinished(t, e, workspace, "restored task")
	cp, err := e.Checkpoint(ctx, restored, "before")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvent(ctx, restored, "turn.accepted", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreCheckpoint(ctx, restored, cp.ID, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{crashed, finished, restored} {
		if err := st.SetSessionStatus(ctx, id, "interrupted"); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := st.LatestRequest(ctx, crashed)
	idle, err := e.CreateIdle(ctx, workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	calls := len(s.requests)
	s.mu.Unlock()

	resumed, err := e.ResumeInterrupted(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0] != crashed {
		t.Fatalf("resumed = %v, want [%s]", resumed, crashed)
	}
	waitForStatus(t, st, crashed, string(agentproto.Pass))
	after, _ := st.LatestRequest(ctx, crashed)
	if after != before {
		t.Fatalf("latest request changed: %q -> %q", before, after)
	}
	msgs, _ := st.Messages(ctx, crashed)
	found := false
	for _, m := range msgs {
		found = found || (m.Role == "user" && strings.Contains(m.ContentJSON, resumeNote))
	}
	if !found {
		t.Fatalf("resume note not stored: %+v", msgs)
	}
	for _, id := range []string{idle.ID, finished, restored} {
		if got, _ := st.Session(ctx, id); got.Status != "interrupted" {
			t.Fatalf("session %s status = %q, want untouched", id, got.Status)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) <= calls {
		t.Fatal("resumed session never reached the model")
	}
}
