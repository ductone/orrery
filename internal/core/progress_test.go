package core

import (
	"errors"
	"strings"
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
	p := newProgressTracker()
	p.phaseTurns = 5
	if p.shouldForcePlanExecution() {
		t.Fatal("plan execution was forced too early")
	}
	p.phaseTurns = 6
	if !p.shouldForcePlanExecution() {
		t.Fatal("plan execution was not forced at the phase limit")
	}
	p.phaseTurns = 1
	p.repeatedTodos = 2
	if !p.shouldForcePlanExecution() {
		t.Fatal("repeated todo updates did not force plan execution")
	}
	if shouldForceFinalResolution("review", 8) || !shouldForceFinalResolution("review", 9) {
		t.Fatal("review final-resolution boundary is not enforced")
	}
	if shouldForceFinalResolution("implement", 12) {
		t.Fatal("implementation phase must not force final resolution")
	}
}

func TestVerifiedCompletionIsForcedAfterReviewWithoutEdits(t *testing.T) {
	p := newProgressTracker()
	p.phase = "review"
	p.verified = true
	p.turnsSinceEdit = 2
	if p.shouldForceVerifiedCompletion() {
		t.Fatal("verified completion was forced too early")
	}
	p.turnsSinceEdit = 3
	if !p.shouldForceVerifiedCompletion() {
		t.Fatal("verified completion was not forced after three review turns without edits")
	}
	p.phase = "diagnose"
	if !p.shouldForceVerifiedCompletion() {
		t.Fatal("verified completion was not forced during diagnosis")
	}
	p.phase = "implement"
	if p.shouldForceVerifiedCompletion() {
		t.Fatal("verified completion must not be forced during implementation")
	}
	p.phase = "review"
	p.verified = false
	if p.shouldForceVerifiedCompletion() {
		t.Fatal("unverified review must not force completion")
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
	if !p.awaitingFix() {
		t.Fatal("an unchanged refusal still awaits a fix")
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

func TestSerializedToolCallResponse(t *testing.T) {
	m := provider.Message{Role: "assistant", Content: `<｜DSML｜tool_calls><｜DSML｜invoke name="read">x</｜DSML｜invoke></｜DSML｜tool_calls>`}
	if !serializedToolCallResponse(m) {
		t.Fatal("serialized DSML tool call accepted as final")
	}
	if serializedToolCallResponse(provider.Message{Role: "assistant", Content: `{"answer":"done"}`}) {
		t.Fatal("ordinary final rejected")
	}
}

func TestUnfinishedFinalResponse(t *testing.T) {
	unfinished := strings.Repeat("I still need the missing prerequisite. I'll search for it. Checking now. ", 20)
	if !unfinishedFinalResponse(provider.Message{Role: "assistant", Content: unfinished}) {
		t.Fatal("work-in-progress reasoning stream accepted as final")
	}
	finished := strings.Repeat("The implementation is complete and the focused tests pass. ", 30)
	if unfinishedFinalResponse(provider.Message{Role: "assistant", Content: finished}) {
		t.Fatal("ordinary long final was rejected")
	}
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
