package core

import (
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
