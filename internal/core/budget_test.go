package core

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

func TestAddBudgetRejectsInvalidAmounts(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "budget test", Phase: "plan", BudgetUSD: 5}); err != nil {
		t.Fatal(err)
	}
	for _, amount := range []float64{0, -1, math.NaN(), math.Inf(1), maxBudgetIncreaseUSD + 1} {
		if _, _, err := e.AddBudget(ctx, sid, amount, nil); err == nil {
			t.Fatalf("expected rejection of %v", amount)
		}
	}
	s, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.BudgetUSD != 5 {
		t.Fatalf("rejected increases must not move the ceiling: %v", s.BudgetUSD)
	}
}

func TestAddBudgetUnknownSession(t *testing.T) {
	e, _ := testEngine(t)
	if _, _, err := e.AddBudget(context.Background(), uuid.NewString(), 5, nil); err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

// A session that stopped for a reason other than its budget gets the raised
// ceiling but must not be restarted by it.
func TestAddBudgetDoesNotResumeNonExhaustedSession(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "budget test", Phase: "plan", BudgetUSD: 5, Status: string(agentproto.Pass)}); err != nil {
		t.Fatal(err)
	}
	s, resumed, err := e.AddBudget(ctx, sid, 7.5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("a passed session must not be resumed by a budget increase")
	}
	if s.BudgetUSD != 12.5 {
		t.Fatalf("budget_usd=%v want 12.5", s.BudgetUSD)
	}
	if after, err := st.Session(ctx, sid); err != nil || after.Status != string(agentproto.Pass) {
		t.Fatalf("status changed: %+v %v", after.Status, err)
	}
}

func TestAddBudgetResumesExhaustedSession(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	workspace := t.TempDir()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-terra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Done with the task."}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer server.Close()
	cfg := config.Config{
		WorkspaceRoot: workspace,
		Providers:     map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: server.URL}},
		Router:        config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"},
	}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)

	req := agentproto.TaskRequest{
		Spec:      "Do a small thing",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute, MaxDepth: 4},
		Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"},
		Depth:     4,
	}
	sid, resultCh, err := e.Start(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("initial run did not finish")
	}
	before := calls.Load()

	// Park the session exactly where a real exhaustion leaves it: spend at the
	// ceiling and the terminal budget_exhausted status.
	s, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	s.SpentUSD, s.BudgetUSD, s.Status = 5, 5, string(agentproto.BudgetExhausted)
	if err := st.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}

	raised, resumed, err := e.AddBudget(ctx, sid, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("an exhausted session must resume on a budget increase")
	}
	if raised.BudgetUSD != 15 {
		t.Fatalf("budget_usd=%v want 15", raised.BudgetUSD)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() > before {
			after, err := st.Session(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status == string(agentproto.BudgetExhausted) {
				continue
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("resumed session never reached the provider (calls before=%d after=%d)", before, calls.Load())
}

// The increase must be visible to the turn loop, which re-reads the ceiling
// from the store rather than from the request it started with.
func TestAddBudgetIsVisibleToStore(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	sid := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: sid, Spec: "budget test", Phase: "plan", BudgetUSD: 2, SpentUSD: 2, Status: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.AddBudget(ctx, sid, 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.AddBudget(ctx, sid, 1.25, nil); err != nil {
		t.Fatal(err)
	}
	s, err := st.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.BudgetUSD != 6.25 {
		t.Fatalf("increases must accumulate: budget_usd=%v want 6.25", s.BudgetUSD)
	}
	if s.SpentUSD != 2 {
		t.Fatalf("spend must be untouched: %v", s.SpentUSD)
	}
	if strings.TrimSpace(s.Status) != "interrupted" {
		t.Fatalf("status changed: %q", s.Status)
	}
}
