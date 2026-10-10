package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/provider"
)

func TestProgressRecordsEveryCommandAfterAnEdit(t *testing.T) {
	p := newProgressTracker()
	edit := provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "ls"}}, map[string]any{"summary": "a b"}, nil)
	if len(p.checksSinceEdit) != 0 || p.verified {
		t.Fatal("commands before any edit are not evidence about it")
	}
	p.observe(edit, nil, nil)
	call := provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "./scripts/check.sh"}}
	output := strings.Repeat("x", 1600) + "\nall checks passed"
	p.observe(call, map[string]any{"summary": output}, nil)
	if len(p.checksSinceEdit) != 1 || p.checksSinceEdit[0].Command != "./scripts/check.sh" || p.checksSinceEdit[0].Output != output[len(output)-1500:] {
		t.Fatalf("checks = %#v", p.checksSinceEdit)
	}
	if !p.verified {
		t.Fatal("a command after an edit marks the change as checked by something; reviewers judge whether it sufficed")
	}
	p.observe(call, map[string]any{"summary": "failed"}, errors.New("exit 1"))
	if len(p.checksSinceEdit) != 1 {
		t.Fatal("failed commands are not recorded as evidence")
	}
	p.observe(edit, nil, nil)
	if len(p.checksSinceEdit) != 0 || p.verified {
		t.Fatal("editing must clear stale evidence")
	}
}
