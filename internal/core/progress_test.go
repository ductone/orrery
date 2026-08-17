package core

import (
	"errors"
	"strings"
	"testing"

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
	if !p.shouldDelegate() || p.repeatedReads != 3 {
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
	if p.noProgressTurns != 4 || !p.shouldNudge() {
		t.Fatalf("tracker=%+v", p)
	}
}

func TestRepeatedUnchangedTodoTerminatesStall(t *testing.T) {
	p := newProgressTracker()
	call := provider.ToolCall{Name: "todo", Arguments: map[string]any{"items": []any{map[string]any{"text": "find missing source", "status": "in_progress"}}}}
	for range 7 {
		p.beginTurn("plan")
		p.observe(call, map[string]any{"phase": "plan"}, nil)
		p.endTurn()
	}
	if p.repeatedTodos != 6 || p.noProgressTurns != 6 {
		t.Fatalf("tracker=%+v", p)
	}
	if got := p.terminalStallReason(); got == "" {
		t.Fatal("repeated unchanged todo did not terminate the stall")
	}
}

func TestRootReviewAndDiagnosisHaveTerminalBounds(t *testing.T) {
	if got := terminalPhaseStallReason("", "plan", 10); got == "" {
		t.Fatal("root plan phase was not bounded")
	}
	if got := terminalPhaseStallReason("child", "plan", 20); got != "" {
		t.Fatalf("child plan phase was incorrectly bounded: %s", got)
	}
	for _, phase := range []string{"review", "diagnose"} {
		if got := terminalPhaseStallReason("", phase, 12); got == "" {
			t.Fatalf("phase %q was not bounded", phase)
		}
		if got := terminalPhaseStallReason("child", phase, 20); got != "" {
			t.Fatalf("child phase %q was incorrectly bounded: %s", phase, got)
		}
	}
	if got := terminalPhaseStallReason("", "implement", 20); got != "" {
		t.Fatalf("implementation incorrectly used review bound: %s", got)
	}
}

func TestBoundedPhaseTransitions(t *testing.T) {
	if shouldForceWorkerSynthesis(3) || !shouldForceWorkerSynthesis(4) {
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

func TestIndependentReviewRemediationBoundSurvivesPhaseChanges(t *testing.T) {
	p := newProgressTracker()
	p.markReviewRejected()
	for i, phase := range []string{"diagnose", "explore", "plan", "implement", "review", "explore", "diagnose"} {
		p.beginTurn(phase)
		if i == 3 {
			p.markReviewRejected()
		}
		if got := p.reviewRemediationReason(""); got != "" {
			t.Fatalf("remediation terminated early at turn %d: %s", i+1, got)
		}
	}
	p.beginTurn("explore")
	if got := p.reviewRemediationReason(""); got == "" {
		t.Fatal("phase changes bypassed independent-review remediation bound")
	}
	if got := p.reviewRemediationReason("child"); got != "" {
		t.Fatalf("child remediation was incorrectly bounded: %s", got)
	}
}

func TestSuccessfulSpawnMarksDelegationAndNudgeIsOncePerPhase(t *testing.T) {
	p := newProgressTracker()
	p.beginTurn("explore")
	p.observe(provider.ToolCall{Name: "spawn"}, map[string]any{"id": "job"}, nil)
	if !p.delegated {
		t.Fatal("successful spawn did not satisfy delegation")
	}
	p.phaseTurns = 7
	if !p.shouldNudge() {
		t.Fatal("expected phase nudge")
	}
	p.markNudged()
	if p.shouldNudge() {
		t.Fatal("nudge repeated in same phase")
	}
	p.beginTurn("implement")
	if p.nudges != 0 {
		t.Fatal("phase change did not reset nudge")
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
