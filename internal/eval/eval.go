package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/store"
)

const SchemaVersion = 1

type Case struct {
	Name         string         `json:"name"`
	Spec         string         `json:"spec"`
	Workspace    string         `json:"workspace,omitempty"`
	Fixture      string         `json:"fixture,omitempty"`
	Acceptance   string         `json:"acceptance"`
	ResultSchema map[string]any `json:"result_schema,omitempty"`
	MaxUSD       float64        `json:"max_usd,omitempty"`
	MaxTokens    int            `json:"max_tokens,omitempty"`
	Timeout      time.Duration  `json:"-"`
	TimeoutText  string         `json:"timeout,omitempty"`
}

func BuildCase(ctx context.Context, s *store.Store, sessionID, acceptance string) (Case, error) {
	session, err := s.Session(ctx, sessionID)
	if err != nil {
		return Case{}, err
	}
	return Case{Name: "session-" + sessionID, Spec: session.Spec, Acceptance: acceptance}, nil
}

type Result struct {
	Name                  string            `json:"name"`
	Passed                bool              `json:"passed"`
	Status                agentproto.Status `json:"status"`
	CostUSD               float64           `json:"cost_usd"`
	LatencySeconds        float64           `json:"latency_seconds"`
	Tokens                int               `json:"tokens"`
	ToolCalls             int               `json:"tool_calls"`
	ToolErrors            int               `json:"tool_errors"`
	EditAttempts          int               `json:"edit_attempts"`
	EditRetries           int               `json:"edit_retries"`
	EditLandRate          float64           `json:"edit_land_rate"`
	NoProgressTurns       int               `json:"no_progress_turns"`
	DuplicateReads        int               `json:"duplicate_reads"`
	DuplicateSearches     int               `json:"duplicate_searches"`
	ProgressNudges        int               `json:"progress_nudges"`
	CompletionRejections  int               `json:"completion_rejections"`
	UsedExplorationWorker bool              `json:"used_exploration_worker"`
	Verified              bool              `json:"verified"`
	IndependentlyReviewed bool              `json:"independently_reviewed"`
	Error                 string            `json:"error,omitempty"`
}

type Summary struct {
	Cases                 int     `json:"cases"`
	Passed                int     `json:"passed"`
	PassRate              float64 `json:"pass_rate"`
	TotalCostUSD          float64 `json:"total_cost_usd"`
	CostPerPassUSD        float64 `json:"cost_per_pass_usd"`
	TotalTokens           int     `json:"total_tokens"`
	MedianTokens          float64 `json:"median_tokens"`
	MedianLatencySeconds  float64 `json:"median_latency_seconds"`
	P95LatencySeconds     float64 `json:"p95_latency_seconds"`
	ToolCalls             int     `json:"tool_calls"`
	ToolErrors            int     `json:"tool_errors"`
	ToolErrorRate         float64 `json:"tool_error_rate"`
	EditAttempts          int     `json:"edit_attempts"`
	EditRetries           int     `json:"edit_retries"`
	EditLandRate          float64 `json:"edit_land_rate"`
	VerifiedRate          float64 `json:"verified_rate"`
	IndependentReviewRate float64 `json:"independent_review_rate"`
}

type Comparison struct {
	BaselineGeneratedAt    time.Time `json:"baseline_generated_at"`
	PassRateRatio          float64   `json:"pass_rate_ratio"`
	CostPerPassChange      float64   `json:"cost_per_pass_change"`
	MedianLatencyChange    float64   `json:"median_latency_change"`
	GuardrailPassRateRatio float64   `json:"guardrail_pass_rate_ratio"`
	Passed                 bool      `json:"passed"`
	Regressions            []string  `json:"regressions,omitempty"`
}

type Report struct {
	SchemaVersion int         `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Policy        string      `json:"policy"`
	Results       []Result    `json:"results"`
	Summary       Summary     `json:"summary"`
	Comparison    *Comparison `json:"comparison,omitempty"`
}

func Load(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	base := filepath.Dir(path)
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c Case
		if err = json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if c.Name == "" || c.Spec == "" || c.Acceptance == "" {
			return nil, fmt.Errorf("%s:%d: name, spec, and acceptance are required", path, line)
		}
		if c.Fixture != "" {
			if !filepath.IsAbs(c.Fixture) {
				c.Fixture = filepath.Join(base, c.Fixture)
			}
			c.Fixture = filepath.Clean(c.Fixture)
		}
		if c.Workspace != "" && !filepath.IsAbs(c.Workspace) {
			c.Workspace = filepath.Clean(filepath.Join(base, c.Workspace))
		}
		if c.TimeoutText != "" {
			c.Timeout, err = time.ParseDuration(c.TimeoutText)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: timeout: %w", path, line, err)
			}
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

func Run(ctx context.Context, engine *core.Engine, policy string, cases []Case) (Report, error) {
	report := Report{SchemaVersion: SchemaVersion, GeneratedAt: time.Now().UTC(), Policy: policy}
	for _, c := range cases {
		workspace, cleanup, err := caseWorkspace(c)
		if err != nil {
			return report, fmt.Errorf("case %s: %w", c.Name, err)
		}
		result := runCase(ctx, engine, policy, c, workspace)
		cleanup()
		report.Results = append(report.Results, result)
	}
	report.Summary = summarize(report.Results)
	return report, nil
}

func runCase(parent context.Context, engine *core.Engine, policy string, c Case, workspace string) Result {
	maxUSD := c.MaxUSD
	if maxUSD <= 0 {
		maxUSD = 5
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2_000_000
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req := agentproto.TaskRequest{Spec: c.Spec, ResultSchema: c.ResultSchema, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write"}, Budget: agentproto.Budget{MaxUSD: maxUSD, MaxTokens: maxTokens, MaxWallClock: timeout, MaxDepth: 4}, Depth: 4}
	if policy == "frontier-pinned" {
		req.Hints.TierPin = "frontier"
	}
	taskResult, runErr := engine.Run(ctx, req, nil)
	outcome := taskResult.Outcome
	result := Result{
		Name: c.Name, Status: taskResult.Status, CostUSD: outcome.CostUSD, LatencySeconds: outcome.Latency.Seconds(), Tokens: outcome.Tokens,
		ToolCalls: outcome.ToolCalls, ToolErrors: outcome.ToolErrors, EditAttempts: outcome.EditAttempts, EditRetries: outcome.EditRetries,
		NoProgressTurns: outcome.NoProgressTurns, DuplicateReads: outcome.DuplicateReads, DuplicateSearches: outcome.DuplicateSearches,
		ProgressNudges: outcome.ProgressNudges, CompletionRejections: outcome.CompletionRejects, UsedExplorationWorker: outcome.ExplorationWorker,
		Verified: outcome.Verified, IndependentlyReviewed: outcome.IndependentlyReviewed, Error: taskResult.Error,
	}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	if result.EditAttempts > 0 {
		result.EditLandRate = 1 - float64(result.EditRetries)/float64(result.EditAttempts)
	}
	result.Passed = taskResult.Status == agentproto.Pass
	if result.Passed {
		cmd := exec.CommandContext(ctx, "sh", "-lc", c.Acceptance)
		cmd.Dir = workspace
		if output, err := cmd.CombinedOutput(); err != nil {
			result.Passed = false
			result.Error = fmt.Sprintf("acceptance: %v: %s", err, truncate(string(output), 8000))
		}
	}
	return result
}

func summarize(results []Result) Summary {
	s := Summary{Cases: len(results)}
	latencies := make([]float64, 0, len(results))
	tokens := make([]float64, 0, len(results))
	verified, reviewed := 0, 0
	for _, result := range results {
		if result.Passed {
			s.Passed++
		}
		if result.Verified {
			verified++
		}
		if result.IndependentlyReviewed {
			reviewed++
		}
		s.TotalCostUSD += result.CostUSD
		s.TotalTokens += result.Tokens
		s.ToolCalls += result.ToolCalls
		s.ToolErrors += result.ToolErrors
		s.EditAttempts += result.EditAttempts
		s.EditRetries += result.EditRetries
		latencies = append(latencies, result.LatencySeconds)
		tokens = append(tokens, float64(result.Tokens))
	}
	if s.Cases > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Cases)
		s.VerifiedRate = float64(verified) / float64(s.Cases)
		s.IndependentReviewRate = float64(reviewed) / float64(s.Cases)
	}
	if s.Passed > 0 {
		s.CostPerPassUSD = s.TotalCostUSD / float64(s.Passed)
	}
	if s.ToolCalls > 0 {
		s.ToolErrorRate = float64(s.ToolErrors) / float64(s.ToolCalls)
	}
	if s.EditAttempts > 0 {
		s.EditLandRate = 1 - float64(s.EditRetries)/float64(s.EditAttempts)
	}
	s.MedianLatencySeconds = percentile(latencies, .5)
	s.P95LatencySeconds = percentile(latencies, .95)
	s.MedianTokens = percentile(tokens, .5)
	return s
}

func Compare(current, baseline Report, minPassRatio float64) Comparison {
	if minPassRatio <= 0 {
		minPassRatio = .97
	}
	c := Comparison{BaselineGeneratedAt: baseline.GeneratedAt, GuardrailPassRateRatio: minPassRatio, Passed: true}
	c.PassRateRatio = ratio(current.Summary.PassRate, baseline.Summary.PassRate)
	c.CostPerPassChange = relativeChange(current.Summary.CostPerPassUSD, baseline.Summary.CostPerPassUSD)
	c.MedianLatencyChange = relativeChange(current.Summary.MedianLatencySeconds, baseline.Summary.MedianLatencySeconds)
	if baseline.Summary.PassRate > 0 && c.PassRateRatio < minPassRatio {
		c.Passed = false
		c.Regressions = append(c.Regressions, fmt.Sprintf("pass rate is %.1f%% of baseline; requires at least %.1f%%", 100*c.PassRateRatio, 100*minPassRatio))
	}
	return c
}

func LoadReport(path string) (Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(b, &report); err != nil {
		return Report{}, err
	}
	if report.SchemaVersion != SchemaVersion {
		return Report{}, fmt.Errorf("unsupported benchmark schema %d", report.SchemaVersion)
	}
	return report, nil
}

func caseWorkspace(c Case) (string, func(), error) {
	if c.Fixture == "" {
		if c.Workspace == "" {
			return "", func() {}, errors.New("fixture or workspace is required")
		}
		return c.Workspace, func() {}, nil
	}
	info, err := os.Stat(c.Fixture)
	if err != nil || !info.IsDir() {
		return "", func() {}, fmt.Errorf("fixture %q is not a directory", c.Fixture)
	}
	destination, err := os.MkdirTemp("", "orrery-benchmark-*")
	if err != nil {
		return "", func() {}, err
	}
	if err := copyDir(c.Fixture, destination); err != nil {
		_ = os.RemoveAll(destination)
		return "", func() {}, err
	}
	return destination, func() { _ = os.RemoveAll(destination) }, nil
}

func copyDir(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("benchmark fixtures may not contain symlinks")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			_ = input.Close()
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr := input.Close()
		closeErr := output.Close()
		return errors.Join(copyErr, inputCloseErr, closeErr)
	})
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	index := max(0, int(math.Ceil(float64(len(values))*quantile))-1)
	return values[index]
}

func ratio(value, baseline float64) float64 {
	if baseline == 0 {
		if value == 0 {
			return 1
		}
		return 0
	}
	return value / baseline
}

func relativeChange(value, baseline float64) float64 {
	if baseline == 0 {
		return 0
	}
	return (value - baseline) / baseline
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
