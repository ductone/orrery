package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
)

// The loop a session fell into: it edited, verified, and then the harness
// disabled tool calls to make it finish; the review failed, the agent could
// only answer in text, and every turn reviewed the identical diff again.
func TestFailedReviewRestoresToolsAndIsNotRepeated(t *testing.T) {
	e, st := testEngine(t)
	workspace, git := gitRepo(t)
	writeFile(t, workspace, "Makefile", "test:\n\t@true\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	reviews := 0
	var afterRejection []map[string]any
	rejected := false
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		if strings.Contains(body["instructions"].(string), "Review this proposed workspace diff") {
			reviews++
			return verdictJSON(false, "main.go: the function returns the wrong value")
		}
		for _, raw := range body["input"].([]any) {
			if m, _ := raw.(map[string]any); m != nil {
				if c, _ := m["content"].(string); strings.Contains(c, "Independent review rejected") || strings.Contains(c, "has not changed since") {
					rejected = true
				}
			}
		}
		if rejected {
			afterRejection = append(afterRejection, body)
		}
		switch n {
		case 1:
			return responsesCall("e1", "edit", map[string]any{"path": "main.go", "hunks": []any{map[string]any{"anchor": "e3b0c442", "delete": 0, "insert": []any{"package main"}}}})
		case 2:
			return responsesCall("x1", "exec", map[string]any{"command": "make test"})
		}
		return responsesText("The findings are not fixed.")
	}}
	srv := s.serve(t)
	cfg := gateConfig(workspace, srv.URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "Write main.go", Budget: agentproto.Budget{MaxUSD: 20, MaxTokens: 10_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	sid, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-results:
	case <-time.After(45 * time.Second):
		t.Fatal("the run must end")
	}
	if reviews != 1 {
		t.Fatalf("an unchanged diff must be reviewed once, got %d reviews", reviews)
	}
	if len(afterRejection) == 0 {
		t.Fatal("the agent must get turns after the rejection")
	}
	for _, body := range afterRejection {
		if body["tool_choice"] == "none" {
			t.Fatal("tool calls must stay available while review findings await a fix")
		}
	}
	var reasons []string
	es, _ := st.EventsAfter(context.Background(), sid, 0)
	for _, ev := range es {
		if ev.Type == "completion.rejected" && strings.Contains(string(ev.Data), "diff unchanged since a failed review") {
			reasons = append(reasons, "unchanged")
		}
	}
	if !slices.Contains(reasons, "unchanged") {
		t.Fatal("repeat completions must be refused without a new review")
	}
	_ = os.Remove(filepath.Join(workspace, "main.go"))
}

func TestAwaitingFix(t *testing.T) {
	p := newProgressTracker()
	p.verified, p.turnsSinceEdit, p.phase = true, 5, "review"
	if !p.shouldForceVerifiedCompletion() || p.awaitingFix() {
		t.Fatal("before any review, verified work may be told to finish")
	}
	p.markReviewRejected(true)
	if !p.awaitingFix() {
		t.Fatal("a rejection awaits a fix")
	}
	p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{"applied": 1}, nil)
	if p.awaitingFix() {
		t.Fatal("an edit answers the rejection")
	}
	p.markReviewRejected(true)
	if !p.awaitingFix() {
		t.Fatal("each new rejection awaits a new fix")
	}
}

func TestRemediationBoundCountsTurnsWithoutFixes(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("review")
	p.markReviewRejected(true)
	for range 6 {
		p.beginTurn("implement")
		p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{}, nil)
		p.markReviewRejected(false)
	}
	if p.reviewRemediationReason("") != "" {
		t.Fatal("turns that edit must not count toward the remediation bound")
	}
	p2 := newProgressTracker()
	p2.beginTurn("review")
	p2.markReviewRejected(true)
	for range 7 {
		p2.beginTurn("implement")
	}
	if !strings.Contains(p2.reviewRemediationReason(""), "without fixing anything") {
		t.Fatalf("eight idle turns end remediation: %q", p2.reviewRemediationReason(""))
	}
	p3 := newProgressTracker()
	for range maxReviewRejections {
		p3.markReviewRejected(true)
	}
	if !strings.Contains(p3.reviewRemediationReason(""), "rejected the change 4 times") {
		t.Fatalf("reason = %q", p3.reviewRemediationReason(""))
	}
	if p3.reviewRemediationReason("job") != "" {
		t.Fatal("workers are bounded by their own budgets")
	}
}
