package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/review"
)

func TestOnlyAcceptedChecksBecomeFindingEvidence(t *testing.T) {
	e, _ := testEngine(t)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", Review: true}}, nil, nil)
	for _, accepted := range []bool{false, true} {
		checks := []commandRecord{{Command: "custom-check", Output: "PASS", Accepted: accepted}, {Command: "git status", Output: "clean"}}
		classifier := e.reviewClassifier(checks).(review.JevClassifier)
		if accepted {
			if classifier.Verification == nil || classifier.Verification.Command != "custom-check" {
				t.Fatalf("accepted evidence = %#v", classifier.Verification)
			}
		} else if classifier.Verification != nil {
			t.Fatalf("unaccepted evidence = %#v", classifier.Verification)
		}
	}
}

func TestVerificationClassifierRecordsAcceptanceAndExcludesFormatting(t *testing.T) {
	ctx := context.Background()
	e, _ := testEngine(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			State map[string]any `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.State["command"] != "custom-check" {
			t.Errorf("unexpected classifier candidate: %#v", req.State["command"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"meaningful_check": map[string]any{"type": "noul", "noul": 0.9}}})
	}))
	t.Cleanup(srv.Close)
	e.ReplaceRuntime(config.Config{Jev: config.JevConfig{APIKey: "k", BaseURL: srv.URL, Review: true}}, nil, nil)
	p := newProgressTracker()
	p.observe(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "feature.go"}}, nil, nil)
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "gofmt -l ."}}, map[string]any{"summary": ""}, nil)
	if e.verificationSatisfied(ctx, "missing-session", t.TempDir(), p, nil) || calls.Load() != 0 {
		t.Fatal("format-only check must not satisfy code verification or be classified")
	}
	p.observe(provider.ToolCall{Name: "exec", Arguments: map[string]any{"command": "custom-check"}}, map[string]any{"summary": "PASS"}, nil)
	if !e.verificationSatisfied(ctx, "missing-session", t.TempDir(), p, nil) || calls.Load() != 1 {
		t.Fatal("custom check must be classified and accepted")
	}
	if len(p.checksSinceEdit) != 1 || !p.checksSinceEdit[0].Accepted {
		t.Fatalf("checks = %#v", p.checksSinceEdit)
	}
	classifier := e.reviewClassifier(p.checksSinceEdit).(review.JevClassifier)
	if classifier.Verification == nil || classifier.Verification.Command != "custom-check" || classifier.Verification.Output != "PASS" {
		t.Fatalf("verification = %#v", classifier.Verification)
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
