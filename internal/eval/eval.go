package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/core"
	"github.com/ductone/orrey/internal/store"
	"os"
	"os/exec"
	"sort"
	"time"
)

type Case struct {
	Name         string         `json:"name"`
	Spec         string         `json:"spec"`
	Workspace    string         `json:"workspace"`
	Acceptance   string         `json:"acceptance"`
	ResultSchema map[string]any `json:"result_schema,omitempty"`
}

func BuildCase(ctx context.Context, s *store.Store, sessionID, acceptance string) (Case, error) {
	session, err := s.Session(ctx, sessionID)
	if err != nil {
		return Case{}, err
	}
	return Case{Name: "session-" + sessionID, Spec: session.Spec, Acceptance: acceptance}, nil
}

type Result struct {
	Name         string            `json:"name"`
	Passed       bool              `json:"passed"`
	Status       agentproto.Status `json:"status"`
	CostUSD      float64           `json:"cost_usd"`
	Latency      time.Duration     `json:"latency"`
	EditLandRate float64           `json:"edit_land_rate"`
	Error        string            `json:"error,omitempty"`
}
type Report struct {
	Policy               string   `json:"policy"`
	Results              []Result `json:"results"`
	PassRate             float64  `json:"pass_rate"`
	TotalCost            float64  `json:"total_cost"`
	MedianLatencySeconds float64  `json:"median_latency_seconds"`
}

func Load(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Case
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c Case
		if err = json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, sc.Err()
}
func Run(ctx context.Context, e *core.Engine, policy string, cases []Case) (Report, error) {
	report := Report{Policy: policy}
	var lat []float64
	for _, c := range cases {
		req := agentproto.TaskRequest{Spec: c.Spec, ResultSchema: c.ResultSchema, Workspace: agentproto.Workspace{Path: c.Workspace, Isolation: "shared"}, Budget: agentproto.Budget{MaxUSD: 25, MaxTokens: 4_000_000, MaxWallClock: 2 * time.Hour, MaxDepth: 4}, Depth: 4}
		if policy == "frontier-pinned" {
			req.Hints.TierPin = "frontier"
		}
		r, err := e.Run(ctx, req, nil)
		x := Result{Name: c.Name, Status: r.Status, CostUSD: r.Outcome.CostUSD, Latency: r.Outcome.Latency, Error: r.Error}
		if err != nil {
			x.Error = err.Error()
		}
		x.Passed = r.Status == agentproto.Pass
		if x.Passed && c.Acceptance != "" {
			cmd := exec.CommandContext(ctx, "sh", "-lc", c.Acceptance)
			cmd.Dir = c.Workspace
			if b, err := cmd.CombinedOutput(); err != nil {
				x.Passed = false
				x.Error = fmt.Sprintf("acceptance: %v: %s", err, b)
			}
		}
		if r.Outcome.ToolCalls > 0 {
			x.EditLandRate = 1 - float64(r.Outcome.EditRetries)/float64(r.Outcome.ToolCalls)
		}
		report.Results = append(report.Results, x)
		if x.Passed {
			report.PassRate++
		}
		report.TotalCost += x.CostUSD
		lat = append(lat, x.Latency.Seconds())
	}
	if len(cases) > 0 {
		report.PassRate /= float64(len(cases))
	}
	sort.Float64s(lat)
	if len(lat) > 0 {
		report.MedianLatencySeconds = lat[len(lat)/2]
	}
	return report, nil
}
