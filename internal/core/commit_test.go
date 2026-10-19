package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

// committingStore is a storage.Store that is also a storage.Committer: it
// counts commits, records them in an order log shared with the test's emit
// callback, and can be made to fail them.
type committingStore struct {
	storage.Store
	mu      sync.Mutex
	commits int
	err     error
	order   *[]string
}

func (c *committingStore) Commit(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits++
	if c.order != nil {
		*c.order = append(*c.order, "commit")
	}
	return c.err
}

func (c *committingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commits
}

func newCommittingEngine(t *testing.T, s *scriptedResponses) (*Engine, *committingStore, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs := &committingStore{Store: st}
	srv := s.serve(t)
	workspace := t.TempDir()
	cfg := gateConfig(workspace, srv.URL)
	cfg.Budget = config.BudgetConfig{SessionUSD: 5, JobDefaultFraction: .2}
	e := New(cfg, cs, provider.New(cfg), nil)
	t.Cleanup(func() { e.Close() })
	return e, cs, workspace
}

func commitTestRequest(workspace string) agentproto.TaskRequest {
	return agentproto.TaskRequest{Spec: "task", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
}

// The engine commits at each step boundary and before the terminal result is
// reported.
func TestCommitAtStepBoundariesBeforeTerminal(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("c1", "todo", map[string]any{"items": []any{}})
		}
		return responsesText("done")
	}}
	e, cs, workspace := newCommittingEngine(t, s)
	var order []string
	cs.order = &order
	var omu sync.Mutex
	res, err := e.Run(context.Background(), commitTestRequest(workspace), func(ev agentproto.AgentEvent) {
		if ev.Type == "terminal" {
			omu.Lock()
			order = append(order, "terminal")
			omu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Pass {
		t.Fatalf("result = %+v", res)
	}
	omu.Lock()
	defer omu.Unlock()
	terminal := -1
	commitsBefore := 0
	for i, o := range order {
		if o == "terminal" {
			terminal = i
			break
		}
		commitsBefore++
	}
	if terminal < 0 {
		t.Fatalf("no terminal event; order=%v", order)
	}
	// One commit per step (two model responses) plus the one at the end of
	// the run; the terminal event must come after all of them.
	if commitsBefore < 3 {
		t.Fatalf("commits before terminal = %d, want at least 3; order=%v", commitsBefore, order)
	}
}

// A store that cannot make a run's writes durable must not report a pass.
func TestCommitFailureFailsRun(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	e, cs, workspace := newCommittingEngine(t, s)
	cs.err = errors.New("flush refused")
	res, err := e.Run(context.Background(), commitTestRequest(workspace), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.Fail || !strings.Contains(res.Error, "commit") || !strings.Contains(res.Error, "flush refused") {
		t.Fatalf("result = %+v, want Fail mentioning the commit failure", res)
	}
}

// A commit that fails mid-run ends the run at that step.
func TestCommitFailureMidRunFailsRun(t *testing.T) {
	s := &scriptedResponses{reply: func(n int, _ map[string]any) map[string]any {
		if n == 1 {
			return responsesCall("c1", "todo", map[string]any{"items": []any{}})
		}
		return responsesText("done")
	}}
	e, cs, workspace := newCommittingEngine(t, s)
	cs.err = errors.New("flush refused")
	res, _ := e.Run(context.Background(), commitTestRequest(workspace), nil)
	if res.Status != agentproto.Fail || !strings.Contains(res.Error, "flush refused") {
		t.Fatalf("result = %+v", res)
	}
	s.mu.Lock()
	n := len(s.requests)
	s.mu.Unlock()
	if n > 1 {
		t.Fatalf("model was called %d times after a failed commit", n)
	}
}

// A session pausing for input commits first, so the pending question is
// durable before the host is told the session is waiting.
func TestCommitBeforeInputRequired(t *testing.T) {
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any {
		return responsesCall("c1", "ask", map[string]any{"question": "Which file?"})
	}}
	e, cs, workspace := newCommittingEngine(t, s)
	var order []string
	cs.order = &order
	var omu sync.Mutex
	res, err := e.Run(context.Background(), commitTestRequest(workspace), func(ev agentproto.AgentEvent) {
		if ev.Type == "input_required" {
			omu.Lock()
			order = append(order, "input_required")
			omu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agentproto.InputRequired {
		t.Fatalf("result = %+v", res)
	}
	omu.Lock()
	defer omu.Unlock()
	i := len(order) - 1
	for ; i >= 0 && order[i] != "input_required"; i-- {
	}
	if i < 1 || order[i-1] != "commit" {
		t.Fatalf("input_required not immediately preceded by a commit; order=%v", order)
	}
}
