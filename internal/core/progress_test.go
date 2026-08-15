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

func isSuppressed(v any) bool {
	m, _ := v.(map[string]any)
	b, _ := m["suppressed"].(bool)
	return b
}
