package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ductone/orrey/internal/core"
)

// ReviewCase is a prepared change for the review benchmark: a fixture at its
// base, a change directory whose files overlay it, and whether the change
// introduces a bug the review should catch.
type ReviewCase struct {
	Name    string `json:"name"`
	Task    string `json:"task"`
	Fixture string `json:"fixture"`
	Change  string `json:"change"`
	// Bug marks a change the review should reject; Why says what is wrong (or,
	// for a clean change, what might look wrong but is not).
	Bug      bool     `json:"bug"`
	Why      string   `json:"why,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

type ReviewResult struct {
	Name      string   `json:"name"`
	Bug       bool     `json:"bug"`
	Approved  bool     `json:"approved"`
	Correct   bool     `json:"correct"`
	Stage     string   `json:"stage"`
	GateScore *float64 `json:"gate_score,omitempty"`
	Findings  []string `json:"findings,omitempty"`
	Seconds   float64  `json:"seconds"`
	CostUSD   float64  `json:"cost_usd"`
	Error     string   `json:"error,omitempty"`
}

type ReviewSummary struct {
	Cases        int `json:"cases"`
	Correct      int `json:"correct"`
	BugCases     int `json:"bug_cases"`
	Caught       int `json:"caught"`
	CleanCases   int `json:"clean_cases"`
	FalseRejects int `json:"false_rejects"`
	// MissedByStage counts buggy changes approved, by the stage that approved
	// them: the gate's misses are what its threshold trades for speed.
	MissedByStage   map[string]int `json:"missed_by_stage"`
	DecidedByStage  map[string]int `json:"decided_by_stage"`
	MedianSeconds   float64        `json:"median_seconds"`
	TotalCostUSD    float64        `json:"total_cost_usd"`
	CatchRate       float64        `json:"catch_rate"`
	FalseRejectRate float64        `json:"false_reject_rate"`
}

type ReviewReport struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Results       []ReviewResult `json:"results"`
	Summary       ReviewSummary  `json:"summary"`
}

func LoadReview(path string) ([]ReviewCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	base := filepath.Dir(path)
	var out []ReviewCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c ReviewCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if c.Name == "" || c.Task == "" || c.Fixture == "" || c.Change == "" {
			return nil, fmt.Errorf("%s:%d: name, task, fixture, and change are required", path, line)
		}
		for _, p := range []*string{&c.Fixture, &c.Change} {
			if !filepath.IsAbs(*p) {
				*p = filepath.Clean(filepath.Join(base, *p))
			}
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// RunReview reviews each prepared change with the engine's review cascade.
func RunReview(ctx context.Context, engine *core.Engine, cases []ReviewCase) (ReviewReport, error) {
	report := ReviewReport{SchemaVersion: SchemaVersion, GeneratedAt: time.Now().UTC()}
	for _, c := range cases {
		workspace, cleanup, err := caseWorkspace(Case{Name: c.Name, Fixture: c.Fixture})
		if err != nil {
			return report, fmt.Errorf("%s: %w", c.Name, err)
		}
		outcome, err := engine.ReviewChange(ctx, workspace, c.Task, func() error { return copyDir(c.Change, workspace) }, c.Commands)
		cleanup()
		r := ReviewResult{Name: c.Name, Bug: c.Bug, Approved: outcome.Passed, Stage: outcome.Stage, GateScore: outcome.GateScore, Findings: outcome.Findings, Seconds: outcome.Seconds, CostUSD: outcome.CostUSD}
		if err != nil {
			r.Error = err.Error()
		}
		r.Correct = err == nil && r.Approved != c.Bug
		report.Results = append(report.Results, r)
	}
	report.Summary = summarizeReview(report.Results)
	return report, nil
}

func summarizeReview(results []ReviewResult) ReviewSummary {
	s := ReviewSummary{Cases: len(results), MissedByStage: map[string]int{}, DecidedByStage: map[string]int{}}
	seconds := make([]float64, 0, len(results))
	for _, r := range results {
		s.DecidedByStage[r.Stage]++
		s.TotalCostUSD += r.CostUSD
		seconds = append(seconds, r.Seconds)
		if r.Correct {
			s.Correct++
		}
		if r.Bug {
			s.BugCases++
			if !r.Approved && r.Error == "" {
				s.Caught++
			} else if r.Approved {
				s.MissedByStage[r.Stage]++
			}
		} else {
			s.CleanCases++
			if !r.Approved {
				s.FalseRejects++
			}
		}
	}
	if s.BugCases > 0 {
		s.CatchRate = float64(s.Caught) / float64(s.BugCases)
	}
	if s.CleanCases > 0 {
		s.FalseRejectRate = float64(s.FalseRejects) / float64(s.CleanCases)
	}
	s.MedianSeconds = percentile(seconds, .5)
	return s
}
