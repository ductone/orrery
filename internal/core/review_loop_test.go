package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	refusals := 0
	checkPending := false
	s := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any {
		if strings.Contains(body["instructions"].(string), "Review this proposed workspace diff") {
			reviews++
			return verdictJSON(false, "main.go: the function returns the wrong value")
		}
		for _, raw := range body["input"].([]any) {
			if m, _ := raw.(map[string]any); m != nil {
				if c, _ := m["content"].(string); strings.Contains(c, "Independent review rejected") || strings.Contains(c, "has not changed since") {
					rejected = true
					if strings.Contains(c, "Independent review rejected") {
						for _, instruction := range []string{"final answer goes to the person, not the reviewer", "describe the whole change made for their request", "not only the fix for these findings"} {
							if !strings.Contains(c, instruction) {
								t.Errorf("review rejection missing %q: %s", instruction, c)
							}
						}
					}
				}
			}
		}
		if rejected {
			afterRejection = append(afterRejection, body)
			if checkPending {
				checkPending = false
				return responsesCall("x"+strconv.Itoa(n), "exec", map[string]any{"command": "make test"})
			}
			refusals++
			// Alternate an unchanged text-only completion with a fresh edit: the
			// unchanged one must be refused without another review, and the
			// fresh diff earns a new independent review until the cap is hit.
			if refusals%2 == 0 {
				checkPending = true
				return responsesCall("e"+strconv.Itoa(n), "edit", map[string]any{"path": "main" + strconv.Itoa(n) + ".go", "hunks": []any{map[string]any{"anchor": "e3b0c442", "delete": 0, "insert": []any{"package main // " + strconv.Itoa(n)}}}})
			}
			return responsesText("The findings are not fixed.")
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
	var result agentproto.TaskResult
	select {
	case result = <-results:
	case <-time.After(45 * time.Second):
		t.Fatal("the run must end")
	}
	input, _ := result.Result["input"].(agentproto.InputRequest)
	if result.Status != agentproto.InputRequired || !strings.HasPrefix(input.ID, limitQuestion) || !strings.Contains(input.Question, "rejected the change 4 times") {
		t.Fatalf("the rejection cap must pause and ask, not end the run: %+v", result)
	}
	if reviews != maxReviewRejections-1 {
		t.Fatalf("each new diff must be reviewed once, got %d reviews", reviews)
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

func TestRemediationCapCountsIndependentRejections(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("review")
	p.markReviewRejected(true)
	for range 6 {
		p.beginTurn("implement")
		p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{}, nil)
		p.markReviewRejected(false)
	}
	if p.reviewRemediationReason("") != "" {
		t.Fatal("unchanged refusals must not count toward the rejection cap")
	}
	p2 := newProgressTracker()
	for range maxReviewRejections {
		p2.markReviewRejected(true)
	}
	if !strings.Contains(p2.reviewRemediationReason(""), "rejected the change 4 times") {
		t.Fatalf("reason = %q", p2.reviewRemediationReason(""))
	}
	if p2.reviewRemediationReason("job") != "" {
		t.Fatal("workers are bounded by their own budgets")
	}
}
