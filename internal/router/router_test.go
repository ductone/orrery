package router

import (
	"context"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
	"strings"
	"testing"
)

type ledger struct{ records []store.RoutingRecord }

func (l *ledger) Cache(context.Context, string, string) (store.CacheEntry, error) {
	return store.CacheEntry{}, nil
}

func TestEmptyAvailableSetRejectsCooledProviders(t *testing.T) {
	p := NewV1(config.RouterConfig{LambdaCost: .35}, &ledger{})
	_, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Phase: Plan, InputTokens: 1000, AvailableModels: []string{}})
	if err == nil || !strings.Contains(err.Error(), "no compatible models") {
		t.Fatalf("error=%v", err)
	}
}
func (l *ledger) WriteRouting(_ context.Context, r store.RoutingRecord) error {
	l.records = append(l.records, r)
	return nil
}
func TestAvailableConstraintAndRecord(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: .35}, l)
	d, x, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Turn: 1, Point: TurnStart, Phase: Plan, InputTokens: 1000, AvailableModels: []string{"openai/gpt-5.6-sol"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "openai/gpt-5.6-sol" || len(l.records) != 1 || x == "" {
		t.Fatalf("decision=%+v records=%d", d, len(l.records))
	}
}
func TestReviewUsesDifferentFamily(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: .35}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: ReviewCreation, Phase: Review, InputTokens: 1000, ImplementerFamily: model.OpenAI, AvailableModels: []string{"openai/gpt-5.6-sol", "anthropic/claude-fable-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Family == model.OpenAI {
		t.Fatalf("same-family reviewer: %s", d.Model.ID)
	}
}
func TestReviewFallsBackToImplementerFamilyWhenOnlyProvider(t *testing.T) {
	p := NewV1(config.RouterConfig{LambdaCost: .35}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: ReviewCreation, Phase: Review, InputTokens: 1000, ImplementerFamily: model.XAI, AvailableModels: []string{"xai/grok-4.6"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "xai/grok-4.6" {
		t.Fatalf("reviewer=%s", d.Model.ID)
	}
}
func TestReviewCrossFamilyOverridesFrontierFloor(t *testing.T) {
	p := NewV1(config.RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"review"}}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: ReviewCreation, Phase: Review, InputTokens: 1000, ImplementerFamily: model.XAI, AvailableModels: []string{"xai/grok-4.6", "together/deepseek-ai/DeepSeek-V4-Flash-0731"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Family != model.DeepSeek {
		t.Fatalf("reviewer=%s", d.Model.ID)
	}
}
func TestFrontierFloor(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"plan"}}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Plan, InputTokens: 1000, AvailableModels: []string{"openai/gpt-5.6-sol", "together/deepseek-ai/DeepSeek-V4-Flash-0731"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Tier != model.Frontier {
		t.Fatalf("floor chose %s", d.Model.ID)
	}
}

func TestDefaultModelBreaksInitialScoreTie(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: .35, DefaultModel: "xai/grok-4.6"}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Plan, InputTokens: 1000, AvailableModels: []string{"xai/grok-4.5", "xai/grok-4.6"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "xai/grok-4.6" {
		t.Fatalf("default-model tie-break chose %s", d.Model.ID)
	}
}

func TestImplementRoutesEfficientAtDefaultWeight(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: .35}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: JobCreation, Phase: Implement, InputTokens: 1000, EstimatedOutput: 4000, AvailableModels: []string{"openai/gpt-5.6-sol", "together/deepseek-ai/DeepSeek-V4-Flash-0731"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Tier != model.Efficient {
		t.Fatalf("implement chose %s", d.Model.ID)
	}
}

// A stall driven by repeated reads/searches should prefer a disciplined
// efficient model over a much pricier frontier one.
func TestReadStallPrefersEfficientEscalation(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: 1.6}, l)
	stall := StallSignals{RepeatedReads: 3, NoProgressTurns: 5, PhaseTurns: 6}
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: Escalation, Phase: Explore, InputTokens: 40000, EstimatedOutput: 4000, Stall: stall, AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-terra"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "openai/gpt-5.6-terra" {
		t.Fatalf("read-stall escalation chose %s, want efficient terra", d.Model.ID)
	}
}

// Hard failures (failed commands / tests) still escalate to frontier.
func TestHardStallPrefersFrontierEscalation(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{LambdaCost: 1.6}, l)
	stall := StallSignals{FailedCommands: 3, TestFailStreak: 2}
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: Escalation, Phase: Diagnose, InputTokens: 40000, EstimatedOutput: 4000, Stall: stall, AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-terra"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Tier != model.Frontier {
		t.Fatalf("hard-stall escalation chose %s, want frontier", d.Model.ID)
	}
}

// With a cold prefix (post-compaction) there is no warm cache to preserve, so
// stickiness must not keep an expensive incumbent ahead of a cheaper fit.
func TestColdPrefixDropsStickiness(t *testing.T) {
	l := &ledger{} // empty cache ledger => all cold
	p := NewV1(config.RouterConfig{LambdaCost: 1.6}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Implement, CurrentModel: "openai/gpt-5.6-sol", InputTokens: 60000, EstimatedOutput: 4000, ToolContinuation: true, AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-terra"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "openai/gpt-5.6-terra" {
		t.Fatalf("cold prefix kept incumbent %s, want cheaper terra", d.Model.ID)
	}
}
