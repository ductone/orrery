package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
)

func TestGatewayRateLimitReroutesWithinTheSameKey(t *testing.T) {
	e, _ := testEngine(t)
	workspace := t.TempDir()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if instructions, _ := body["instructions"].(string); !strings.Contains(instructions, systemPromptLead) {
			_ = json.NewEncoder(w).Encode(responsesText("Title"))
			return
		}
		calls = append(calls, body["model"].(string))
		if len(calls) == 1 {
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(responsesText("done"))
	}))
	defer srv.Close()
	cfg := config.Config{WorkspaceRoot: workspace, Providers: map[string]config.ProviderConfig{"ramp": {APIKey: "test", BaseURL: srv.URL}}}
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	req := agentproto.TaskRequest{Spec: "answer", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, results, err := e.Start(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.Status != agentproto.Pass || len(calls) != 2 || calls[0] == calls[1] {
		t.Fatalf("result = %+v, model calls = %v", result, calls)
	}
}

func TestKeepGoingWaitsForRemainingRouteBackoff(t *testing.T) {
	e, _ := testEngine(t)
	workspace := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if instructions, _ := body["instructions"].(string); strings.HasPrefix(instructions, "cool") {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(responsesText("done"))
	}))
	defer srv.Close()
	cfg := config.Config{WorkspaceRoot: workspace, Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}, Router: config.RouterConfig{DisableSwitch: true, DefaultModel: "openai/gpt-5.6-terra"}}
	providers := provider.New(cfg)
	e.ReplaceRuntime(cfg, providers, nil)
	req := agentproto.TaskRequest{Spec: "answer", Budget: agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
	sid, results, err := e.Start(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Status != agentproto.Pass {
		t.Fatalf("initial run = %+v", result)
	}
	spec, _ := model.Get(cfg.Router.DefaultModel)
	_, err = providers.CompleteOne(context.Background(), router.Decision{Model: spec}, func(model.ModelSpec, router.Decision) (provider.Request, error) {
		return provider.Request{System: "cool", MaxOutput: 10}, nil
	})
	if err == nil {
		t.Fatal("expected rate limit")
	}
	paused := e.askAboutLimit(sid, "Routes cooling down. Keep going?", agentproto.Outcome{}, nil)
	if paused.Status != agentproto.InputRequired {
		t.Fatalf("pause = %+v", paused)
	}
	start := time.Now()
	results, err = e.Continue(context.Background(), sid, choiceContinue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Status != agentproto.Pass || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("continued run = %+v, waited = %v", result, time.Since(start))
	}
}

func TestDecideWaitingUsesEarliestCompatibleCoolingRoute(t *testing.T) {
	e, _ := testEngine(t)
	first, _ := model.Get("openai/gpt-5.6-terra")
	second := first
	second.ID = "openai/second"
	model.Install([]model.ModelSpec{first, second})
	t.Cleanup(func() { model.Install(model.Catalog) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		wait := "300"
		if body["model"] == "second" {
			wait = "1"
		}
		w.Header().Set("Retry-After", wait)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg := config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test", BaseURL: srv.URL}}}
	providers := provider.New(cfg)
	for _, spec := range model.All() {
		_, _ = providers.CompleteOne(context.Background(), router.Decision{Model: spec}, func(model.ModelSpec, router.Decision) (provider.Request, error) {
			return provider.Request{MaxOutput: 10}, nil
		})
	}
	state := router.RoutingState{SessionID: "test", Phase: router.Explore, AvailableModels: providers.AvailableIDs()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	decision, _, err := e.decideWaiting(ctx, "test", router.NewV1(cfg.Router, e.store), providers, &state, nil)
	if err != nil || decision.Model.ID != second.ID || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("decision = %+v, err = %v, waited = %v", decision, err, time.Since(start))
	}
}
