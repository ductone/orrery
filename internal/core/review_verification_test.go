package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/review"
)

func TestOnlyRecognizedChecksBecomeFindingEvidence(t *testing.T) {
	e, _ := testEngine(t)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", Review: true}}, nil, nil)
	for _, command := range []string{"custom-check", "go test ./..."} {
		checks := []commandRecord{{Command: command, Output: "PASS"}, {Command: "git status", Output: "clean"}}
		classifier := e.reviewClassifier(checks).(review.JevClassifier)
		if command == "go test ./..." {
			if classifier.Verification == nil || classifier.Verification.Command != command {
				t.Fatalf("recognized evidence = %#v", classifier.Verification)
			}
		} else if classifier.Verification != nil {
			t.Fatalf("unrecognized evidence = %#v", classifier.Verification)
		}
	}
}

func TestFormatCheckDoesNotSatisfyCodeVerification(t *testing.T) {
	e, _ := testEngine(t)
	p := newProgressTracker()
	p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, nil, nil)
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "gofmt -l ."}}, map[string]any{"summary": ""}, nil)
	if e.verificationSatisfied(context.Background(), "missing-session", t.TempDir(), p, nil) || p.verified {
		t.Fatal("format-only check must not satisfy code verification")
	}
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "go test ./..."}}, map[string]any{"summary": "PASS"}, nil)
	if !p.verified || len(p.checksSinceEdit) != 1 {
		t.Fatalf("verified = %v, checks = %#v", p.verified, p.checksSinceEdit)
	}
}

func TestFindingsReceiveLatestVerification(t *testing.T) {
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
					if _, ok := state["verification"]; ok {
						t.Error("verification belongs only in finding state")
					}
					return 0.1
				}
				seen.Store(true)
				verification, ok := state["verification"].(map[string]any)
				if withCheck {
					if !ok || verification["command"] != "go test ./internal/..." || verification["output"] != "ok internal/feature" {
						t.Errorf("verification = %#v", state["verification"])
					}
				} else if _, exists := state["verification"]; exists {
					t.Errorf("unexpected verification = %#v", state["verification"])
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

func TestProgressRetainsRecognizedVerificationEvidence(t *testing.T) {
	p := newProgressTracker()
	edit := provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}
	p.observe(edit, nil, nil)
	call := provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "go test ./..."}}
	output := strings.Repeat("x", 1600) + "\nok feature"
	p.observe(call, map[string]any{"summary": output}, nil)
	if len(p.checksSinceEdit) != 1 || p.checksSinceEdit[0].Command != "go test ./..." || p.checksSinceEdit[0].Output != output[len(output)-1500:] {
		t.Fatalf("checks = %#v", p.checksSinceEdit)
	}
	if !p.verified {
		t.Fatal("recognized check must still mark verification")
	}
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "gofmt -l ."}}, map[string]any{"summary": ""}, nil)
	if len(p.checksSinceEdit) != 1 || !p.formatVerified {
		t.Fatal("format checks must retain their separate gate without entering classifier candidates")
	}
	p.observe(call, map[string]any{"summary": "failed"}, errors.New("exit 1"))
	if len(p.checksSinceEdit) != 1 {
		t.Fatal("failed commands must not replace successful verification evidence")
	}
	p.observe(edit, nil, nil)
	if len(p.checksSinceEdit) != 0 {
		t.Fatal("editing must clear stale evidence")
	}
}
