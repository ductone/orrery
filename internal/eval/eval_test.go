package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummarizeEngineeringMetrics(t *testing.T) {
	summary := summarize([]Result{
		{Passed: true, CostUSD: 2, LatencySeconds: 10, Tokens: 100, ToolCalls: 5, ToolErrors: 1, EditAttempts: 2, EditRetries: 1, Verified: true, IndependentlyReviewed: true},
		{Passed: false, CostUSD: 1, LatencySeconds: 30, Tokens: 300, ToolCalls: 5, ToolErrors: 0},
	})
	if summary.PassRate != .5 || summary.CostPerPassUSD != 3 || summary.MedianLatencySeconds != 10 || summary.P95LatencySeconds != 30 {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.ToolErrorRate != .1 || summary.EditLandRate != .5 || summary.VerifiedRate != .5 || summary.IndependentReviewRate != .5 {
		t.Fatalf("quality metrics=%+v", summary)
	}
}

func TestCompareEnforcesPassRateGuardrail(t *testing.T) {
	baseline := Report{GeneratedAt: time.Unix(1, 0), Summary: Summary{PassRate: 1, CostPerPassUSD: 2, MedianLatencySeconds: 10}}
	current := Report{Summary: Summary{PassRate: .96, CostPerPassUSD: 1, MedianLatencySeconds: 5}}
	comparison := Compare(current, baseline, .97)
	if comparison.Passed || len(comparison.Regressions) != 1 || comparison.PassRateRatio != .96 {
		t.Fatalf("comparison=%+v", comparison)
	}
	current.Summary.PassRate = .98
	if comparison = Compare(current, baseline, .97); !comparison.Passed {
		t.Fatalf("comparison=%+v", comparison)
	}
}

func TestLoadResolvesFixtureRelativeToSet(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture")
	if err := os.Mkdir(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	set := filepath.Join(dir, "cases.jsonl")
	line := `{"name":"case","spec":"fix it","fixture":"fixture","acceptance":"go test ./...","timeout":"2m"}` + "\n"
	if err := os.WriteFile(set, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Fixture != fixture || cases[0].Timeout != 2*time.Minute {
		t.Fatalf("cases=%+v", cases)
	}
}

func TestCaseWorkspaceCopiesFixture(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, cleanup, err := caseWorkspace(Case{Fixture: source})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if workspace == source || !strings.Contains(workspace, "orrery-benchmark-") {
		t.Fatalf("workspace=%q", workspace)
	}
	if err := os.WriteFile(filepath.Join(workspace, "file.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(source, "file.txt"))
	if string(original) != "original" {
		t.Fatalf("fixture mutated: %q", original)
	}
}
