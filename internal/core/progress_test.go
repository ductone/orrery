package core

import (
	"errors"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
)

func TestProgressSuppressesUnchangedReadsAndDetectsStall(t *testing.T) {
	p := newProgressTracker()
	call := provider.ToolCall{Name: "read", Arguments: map[string]any{"path": "x.go"}}
	p.beginTurn("explore")
	if got := p.observe(call, []string{"same"}, nil); isSuppressed(got) {
		t.Fatal("first read suppressed")
	}
	p.endTurn()
	for range 3 {
		p.beginTurn("explore")
		if got := p.observe(call, []string{"same"}, nil); !isSuppressed(got) {
			t.Fatal("duplicate read was not suppressed")
		}
		p.endTurn()
	}
	if p.repeatedReads != 3 {
		t.Fatalf("tracker=%+v", p)
	}
}

func TestProgressRecognizesEditsAndVerification(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("implement")
	p.observe(provider.ToolCall{Name: "edit"}, map[string]any{"applied": 1}, nil)
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "make typecheck/frontend"}}, map[string]any{"ok": true}, nil)
	p.endTurn()
	if !p.edited || !p.verified || p.noProgressTurns != 0 {
		t.Fatalf("tracker=%+v", p)
	}
}

func TestProgressTreatsWritingToolsAsEdits(t *testing.T) {
	cases := []struct {
		call  provider.ToolCall
		value any
		paths []string
	}{
		{provider.ToolCall{Name: "write", Arguments: map[string]any{"path": "w.go"}}, map[string]any{"path": "/ws/w.go"}, []string{"w.go"}},
		{provider.ToolCall{Name: "replace", Arguments: map[string]any{"paths": []any{"pkg"}}}, map[string]any{"replaced": []map[string]any{{"path": "/ws/pkg/a.go"}, {"path": "/ws/pkg/b.go"}}}, []string{"/ws/pkg/a.go", "/ws/pkg/b.go"}},
		{provider.ToolCall{Name: "ast_edit", Arguments: map[string]any{"paths": []any{"*.go"}}}, map[string]any{"files": []map[string]any{{"path": "/ws/c.go"}}}, []string{"/ws/c.go"}},
	}
	for _, c := range cases {
		p := newProgressTracker()
		p.verified, p.reviewed = true, true
		p.observe(c.call, c.value, nil)
		if !p.edited || p.verified || p.reviewed {
			t.Fatalf("%s must invalidate verification and review: %+v", c.call.Name, p)
		}
		for _, path := range c.paths {
			if !p.editedPaths[path] {
				t.Fatalf("%s did not record %s: %v", c.call.Name, path, p.editedPaths)
			}
		}
		if !isEditTool(c.call.Name) {
			t.Fatalf("isEditTool(%q) = false", c.call.Name)
		}
	}
}

func TestDiffWhitespaceCheckDoesNotClaimWorkspaceVerification(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("review")
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "git diff --check"}}, map[string]any{"ok": true}, nil)
	p.endTurn()
	if p.verified || p.turnVerified {
		t.Fatalf("diff whitespace check marked workspace verified: %+v", p)
	}
}

func TestFailedVerificationDoesNotClaimSuccess(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("review")
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "go test ./..."}}, nil, errors.New("exit status 1"))
	p.endTurn()
	if p.verified || p.turnVerified {
		t.Fatalf("failed test marked workspace verified: %+v", p)
	}
}

func TestUnchangedTodoDoesNotResetStallDetection(t *testing.T) {
	p := newProgressTracker()
	call := provider.ToolCall{Name: "todo", Arguments: map[string]any{"items": []any{map[string]any{"text": "explore", "status": "in_progress"}}}}
	p.beginTurn("explore")
	if got := p.observe(call, map[string]any{"phase": "explore"}, nil); isSuppressed(got) {
		t.Fatal("first todo update suppressed")
	}
	p.endTurn()
	for range 4 {
		p.beginTurn("explore")
		if got := p.observe(call, map[string]any{"phase": "explore"}, nil); !isSuppressed(got) {
			t.Fatal("unchanged todo was treated as progress")
		}
		p.endTurn()
	}
	if p.noProgressTurns != 4 {
		t.Fatalf("tracker=%+v", p)
	}
}

func TestBoundedPhaseTransitions(t *testing.T) {
	if workerTurnLimit(agentproto.TaskRequest{}) != 4 {
		t.Fatal("read-only worker synthesis boundary is not enforced")
	}
}

func TestIndependentReviewRejectionCapSurvivesPhaseChanges(t *testing.T) {
	p := newProgressTracker()
	for i := range maxReviewRejections {
		p.beginTurn("review")
		p.markReviewRejected(true)
		if i == maxReviewRejections-1 {
			break
		}
		for _, phase := range []string{"diagnose", "explore", "plan", "implement"} {
			p.beginTurn(phase)
		}
		p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{}, nil)
		if got := p.reviewRemediationReason(""); got != "" {
			t.Fatalf("cap fired after only %d rejections: %s", i+1, got)
		}
	}
	if got := p.reviewRemediationReason(""); got == "" {
		t.Fatal("phase changes bypassed the independent-review rejection cap")
	}
	if got := p.reviewRemediationReason("child"); got != "" {
		t.Fatalf("child remediation was incorrectly bounded: %s", got)
	}
}

func TestReviewRemediationCyclesDoNotReachTheCapEarly(t *testing.T) {
	// Each cycle rejects a fresh diff and then answers it with edits a few
	// turns later. Only repeated independent rejections are capped, so a
	// review-fix cycle that fixes the findings must not trip it.
	p := newProgressTracker()
	for cycle := range maxReviewRejections - 1 {
		p.beginTurn("review")
		p.markReviewRejected(true)
		for range 3 {
			p.beginTurn("implement")
		}
		p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{}, nil)
		if got := p.reviewRemediationReason(""); got != "" {
			t.Fatalf("cycle %d tripped the cap after a fix: %s", cycle+1, got)
		}
	}
	if p.reviewRejections != maxReviewRejections-1 {
		t.Fatalf("cycles counted %d rejections, want %d", p.reviewRejections, maxReviewRejections-1)
	}
}

func TestUnchangedSubmissionsDoNotCountAsRejections(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("review")
	p.markReviewRejected(true)
	for turn := 2; turn <= 8; turn++ {
		p.beginTurn("review")
		// Even a successful edit may leave the rejected diff unchanged.
		p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "a.go"}}, map[string]any{}, nil)
		p.markReviewRejected(false)
		if got := p.reviewRemediationReason(""); got != "" {
			t.Fatalf("unchanged submission at turn %d ended remediation early: %s", turn, got)
		}
	}
	if p.reviewRejections != 1 {
		t.Fatalf("unchanged submissions counted as reviews: %d", p.reviewRejections)
	}
}

func TestSuccessfulSpawnMarksDelegation(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("explore")
	p.observe(provider.ToolCall{Name: "spawn"}, map[string]any{"id": "job"}, nil)
	if !p.delegated {
		t.Fatal("successful spawn did not satisfy delegation")
	}
}

func isSuppressed(v any) bool {
	m, _ := v.(map[string]any)
	b, _ := m["suppressed"].(bool)
	return b
}

func TestParseResultUsesTrailingStructuredVerdict(t *testing.T) {
	got := parseResult("Evidence summary with an example {\"pass\":false}.\n\n```json\n{\"pass\":true,\"findings\":[]}\n```")
	if pass, _ := got["pass"].(bool); !pass {
		t.Fatalf("trailing verdict was not parsed: %#v", got)
	}
	if findings, ok := got["findings"].([]any); !ok || len(findings) != 0 {
		t.Fatalf("findings were not parsed: %#v", got)
	}
}
