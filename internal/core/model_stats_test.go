package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
)

func TestMainLoopRecordsModelStats(t *testing.T) {
	e, st := testEngine(t)
	script := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any { return responsesText("Hello!") }}
	srv := script.serve(t)
	cfg := config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}, Router: config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	_, results, err := e.Start(context.Background(), agentproto.TaskRequest{Spec: "Say hello", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 10000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: t.TempDir(), Ownership: "external"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Status != agentproto.Pass {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	stats, err := st.ModelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Route != "openai/gpt-5.6-terra" || stats[0].Calls != 1 || stats[0].LatencySeconds <= 0 || stats[0].OutputTokensPerSecond <= 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestRoutingStateCarriesPerformanceFromStore(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	const route = "openai/gpt-5.6-terra"
	if err := st.RecordModelCall(ctx, route, 12*time.Second, 100, false, 1000, 800); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordModelFailure(ctx, route, "empty"); err != nil {
		t.Fatal(err)
	}
	script := &scriptedResponses{reply: func(n int, body map[string]any) map[string]any { return responsesText("Hello!") }}
	srv := script.serve(t)
	cfg := config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}, Router: config.RouterConfig{DisableSwitch: true, DefaultModel: route}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	_, results, err := e.Start(ctx, agentproto.TaskRequest{Spec: "Say hello", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 10000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: t.TempDir(), Ownership: "external"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Status != agentproto.Pass {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	// routingStates requires a session id; read state_json directly.
	var raw string
	if err := st.DB().QueryRowContext(ctx, `SELECT state_json FROM routing_records ORDER BY created_at DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state router.RoutingState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	perf, ok := state.Performance[route]
	if !ok || perf.Calls < 1 || perf.LatencySeconds <= 0 {
		t.Fatalf("performance=%+v state=%s", state.Performance, raw)
	}
	if perf.FailureRate <= 0 {
		t.Fatalf("expected failure rate from recorded empty failure, got %+v", perf)
	}
}
