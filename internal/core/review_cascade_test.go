package core

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/review"
)

func TestJevGateApprovesASmallChange(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 1 }\n")
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.gate = func(state map[string]any) float64 {
		if _, ok := state["commands_since_last_edit"]; !ok {
			t.Error("the gate must see the commands run since the last edit")
		}
		return 0.9
	}
	h.reviewer = func(string, int) map[string]any { t.Fatal("no full reviewer should run"); return nil }
	h.light = func(string) map[string]any { t.Fatal("no light reviewer should run"); return nil }
	passed, text, err := h.run(commandRecord{Command: "go test ./...", Output: "ok"})
	if err != nil || !passed || !strings.Contains(text, "jev gate") {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
	if g := h.events("review.gate"); len(g) != 1 || g[0]["approved"] != true {
		t.Fatalf("gate events = %v", g)
	}
}

func TestLightReviewApprovesWhenTheGateEscalates(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 1 }\n")
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.gate = func(map[string]any) float64 { return 0.3 }
	h.reviewer = func(string, int) map[string]any { t.Fatal("no full reviewer should run"); return nil }
	h.light = func(string) map[string]any { return verdictJSON(true) }
	passed, _, err := h.run(commandRecord{Command: "./scripts/check.sh", Output: "all 12 checks passed"})
	if err != nil || !passed || len(h.lightSpecs) != 1 {
		t.Fatalf("passed=%v err=%v light=%d", passed, err, len(h.lightSpecs))
	}
	jobs, _ := h.st.Jobs(context.Background(), h.sid)
	var hints agentproto.RoutingHints
	if len(jobs) != 1 || json.Unmarshal([]byte(jobs[0].HintsJSON), &hints) != nil || hints.Effort != review.LightReviewEffort || hints.WorkerTurns != review.LightReviewTurns {
		t.Fatalf("light review job hints = %+v", hints)
	}
	spec := h.lightSpecs[0]
	for _, want := range []string{"Add the feature", "COMMANDS RUN SINCE THE LAST EDIT", "./scripts/check.sh", "all 12 checks passed", "diff --git a/internal/feature.go"} {
		if !strings.Contains(spec, want) {
			t.Errorf("light spec missing %q", want)
		}
	}
}

func TestLightReviewRejectsAConcreteBug(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 1 }\n")
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.reviewer = func(string, int) map[string]any { t.Fatal("no full reviewer should run"); return nil }
	h.light = func(string) map[string]any { return verdictJSON(false, "feature.go:3 returns 1 for every input") }
	passed, text, err := h.run()
	if err != nil || passed || !strings.Contains(text, "returns 1 for every input") {
		t.Fatalf("passed=%v text=%s err=%v", passed, text, err)
	}
}

func TestLightReviewEscalationRunsTheFullReview(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 1 }\n")
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.reviewer = func(string, int) map[string]any { return verdictJSON(true) }
	passed, _, err := h.run()
	if err != nil || !passed || len(h.lightSpecs) != 1 || len(h.specs) != 1 {
		t.Fatalf("passed=%v err=%v light=%d full=%d", passed, err, len(h.lightSpecs), len(h.specs))
	}
	if esc := h.events("review.escalated"); len(esc) != 1 || esc[0]["from"] != "light" {
		t.Fatalf("escalations = %v", esc)
	}
}

func TestGateMayNotApproveRemovedTests(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/feature_test.go", "package internal\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "tests"}} {
		if out, err := exec.Command("git", append([]string{"-C", h.workspace, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	h.write("internal/feature_test.go", "package internal\n\nfunc TestA(t *testing.T) {}\n")
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.gate = func(map[string]any) float64 { t.Error("the gate must not be asked about removed tests"); return 1 }
	h.light = func(string) map[string]any { return verdictJSON(true) }
	h.reviewer = func(string, int) map[string]any { t.Fatal("no full reviewer should run"); return nil }
	if passed, _, err := h.run(); err != nil || !passed || len(h.lightSpecs) != 1 {
		t.Fatalf("passed=%v err=%v light=%d", passed, err, len(h.lightSpecs))
	}
}

func TestLargeChangesGoStraightToTheFullReview(t *testing.T) {
	h := newReviewHarness(t, true)
	h.write("internal/big.go", "package internal\n\n"+strings.Repeat("var _ = 1\n", review.CascadeMaxLines+10))
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.gate = func(map[string]any) float64 { t.Error("the gate must not see a large change"); return 1 }
	h.light = func(string) map[string]any { t.Error("no light reviewer for a large change"); return verdictJSON(true) }
	h.reviewer = func(string, int) map[string]any { return verdictJSON(true) }
	if passed, _, err := h.run(); err != nil || !passed || len(h.specs) != 1 {
		t.Fatalf("passed=%v err=%v full=%d", passed, err, len(h.specs))
	}
	if esc := h.events("review.escalated"); len(esc) != 1 || esc[0]["from"] != "size" {
		t.Fatalf("escalations = %v", esc)
	}
}

func TestReviewChangeReviewsAPreparedChangeWithItsEvidence(t *testing.T) {
	h := newReviewHarness(t, true)
	h.jev = func(map[string]any, string) float64 { return 0.1 }
	h.reviewer = func(string, int) map[string]any { t.Fatal("no full reviewer should run"); return nil }
	h.light = func(string) map[string]any { return verdictJSON(false, "feature.go:3 off by one") }
	out, err := h.e.ReviewChange(context.Background(), h.workspace, "Add the feature", func() error {
		h.write("internal/feature.go", "package internal\n\nfunc Feature() int { return 2 }\n")
		return nil
	}, []string{"echo evidence-ran", "false"})
	if err != nil || out.Passed || out.Stage != "light" || len(out.Findings) != 1 || out.GateScore == nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if spec := h.lightSpecs[0]; !strings.Contains(spec, "evidence-ran") || strings.Contains(spec, "$ false") {
		t.Fatal("successful commands are evidence; failed ones are not")
	}
}
