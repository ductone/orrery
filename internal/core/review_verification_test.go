package core

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/provider"
)

func TestFindingsReceiveCommandEvidence(t *testing.T) {
	for _, withCheck := range []bool{false, true} {
		name := "without check"
		if withCheck {
			name = "latest check"
		}
		t.Run(name, func(t *testing.T) {
			h := newReviewHarness(t, true)
			h.write("internal/feature.go", "package internal\n")
			var seen atomic.Bool
			h.jev = func(state map[string]any, question string) float64 {
				if question != "real_bug" {
					if _, ok := state["commands_since_last_edit"]; ok && question != "approve" {
						t.Error("command evidence belongs only in finding and gate state")
					}
					return 0.1
				}
				seen.Store(true)
				commands, ok := state["commands_since_last_edit"].([]any)
				if withCheck {
					found := false
					for _, c := range commands {
						if m, _ := c.(map[string]any); m["command"] == "go test ./internal/..." && m["output"] == "ok internal/feature" {
							found = true
						}
					}
					if !ok || len(commands) != 3 || !found {
						t.Errorf("commands = %#v", state["commands_since_last_edit"])
					}
				} else if _, exists := state["commands_since_last_edit"]; exists {
					t.Errorf("unexpected commands = %#v", state["commands_since_last_edit"])
				}
				return 0.9
			}
			h.reviewer = func(string, int) map[string]any { return verdictJSON(false, "feature.go: unsupported bug") }
			var checks []commandRecord
			if withCheck {
				p := newProgressTracker()
				p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, nil, nil)
				for _, check := range []commandRecord{{Command: "go test ./...", Output: "old output"}, {Command: "go test ./internal/...", Output: "ok internal/feature"}, {Command: "git diff --stat", Output: "feature.go | 1 +"}} {
					p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": check.Command}}, map[string]any{"summary": check.Output}, nil)
				}
				checks = p.checksSinceEdit
			}
			if _, _, err := h.run(checks...); err != nil {
				t.Fatal(err)
			}
			if !seen.Load() {
				t.Fatal("no finding classification observed")
			}
		})
	}
}

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
