package router

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
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

func TestPinnedDefaultBypassesFrontierFloor(t *testing.T) {
	p := NewV1(config.RouterConfig{
		LambdaCost:          .35,
		DefaultModel:        "openai/gpt-5.6-luna",
		DisableSwitch:       true,
		FrontierFloorPhases: []string{"plan"},
	}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{
		SessionID:       "s",
		Point:           TurnStart,
		Phase:           Plan,
		InputTokens:     1000,
		AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-luna"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "openai/gpt-5.6-luna" {
		t.Fatalf("pinned default chose %s", d.Model.ID)
	}
}

func TestDefaultModelBreaksInitialScoreTie(t *testing.T) {
	l := &ledger{}
	// No cost weight: the two routes differ only in cache-read price, and this
	// tests the tie-break, not pricing.
	p := NewV1(config.RouterConfig{DefaultModel: "xai/grok-4.6"}, l)
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

func routingCatalog() []model.ModelSpec {
	opus, _ := model.Get("ramp/claude-opus-5-5")
	sonnet, _ := model.Get("ramp/claude-sonnet-5-5")
	nano := sonnet
	nano.ID, nano.Family, nano.Discovered = "ramp/gpt-5.4-nano", model.OpenAI, true
	nano.Pricing = model.Pricing{Input: .05, Output: .5, CacheRead: .005}
	return []model.ModelSpec{opus, sonnet, nano}
}

func decideWith(t *testing.T, catalog []model.ModelSpec, s RoutingState) Decision {
	t.Helper()
	// Decisions resolve their model from the active catalog.
	model.Install(catalog)
	t.Cleanup(func() { model.Install(model.Catalog) })
	p := &V1{cfg: config.RouterConfig{LambdaCost: .35}, ledger: &ledger{}, catalog: catalog, now: time.Now}
	ids := make([]string, len(catalog))
	for i, m := range catalog {
		ids[i] = m.ID
	}
	s.SessionID, s.AvailableModels = "s", ids
	if s.InputTokens == 0 {
		s.InputTokens = 40_000
	}
	d, _, err := p.Decide(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDiscoveredModelsDoNotWinOnPriceAlone(t *testing.T) {
	d := decideWith(t, routingCatalog(), RoutingState{Point: TurnStart, Phase: Implement})
	if d.Model.ID == "ramp/gpt-5.4-nano" {
		t.Fatal("a discovered model must not outscore built-in models on price alone")
	}
	// A config override that sets the tier vouches for it.
	c := routingCatalog()
	c[2].Discovered = false
	if d := decideWith(t, c, RoutingState{Point: TurnStart, Phase: Implement}); d.Model.ID != "ramp/gpt-5.4-nano" {
		t.Fatalf("a vouched-for cheap efficient model may win implementation turns, got %s", d.Model.ID)
	}
}

func TestReviewFindingsNeedAFrontierModel(t *testing.T) {
	c := routingCatalog()
	c[2].Discovered = false
	d := decideWith(t, c, RoutingState{Point: TurnStart, Phase: Implement, Stall: StallSignals{ReviewRejected: true}})
	if d.Model.Tier != model.Frontier {
		t.Fatalf("fixing review findings routed to %s (%s)", d.Model.ID, d.Model.Tier)
	}
	// Workers created for other purposes are not affected.
	if d := decideWith(t, c, RoutingState{Point: JobCreation, Phase: Explore, Stall: StallSignals{ReviewRejected: true}}); d.Model.Tier == model.Frontier && d.Model.ID != "ramp/claude-opus-5-5" {
		t.Fatalf("unexpected %s", d.Model.ID)
	}
}

func candidateByModel(d Decision, id string) (Candidate, bool) {
	for _, c := range d.Candidates {
		if c.Model == id {
			return c, true
		}
	}
	return Candidate{}, false
}

func TestSlowRouteLosesToFasterSlightlyPricierRoute(t *testing.T) {
	// Same-tier efficient models; cheap slow route would win on price alone.
	slow, _ := model.Get("ramp/claude-sonnet-5-5")
	fast, _ := model.Get("openai/gpt-5.6-terra")
	catalog := []model.ModelSpec{slow, fast}
	base := decideWith(t, catalog, RoutingState{Point: TurnStart, Phase: Implement})
	if base.Model.ID != slow.ID {
		t.Fatalf("without stats expected cheaper %s, got %s", slow.ID, base.Model.ID)
	}
	d := decideWith(t, catalog, RoutingState{
		Point: TurnStart, Phase: Implement,
		Performance: map[string]RoutePerformance{
			slow.ID: {Calls: 100, OutputTokensPerSecond: 9},
			fast.ID: {Calls: 100, OutputTokensPerSecond: 179},
		},
	})
	if d.Model.ID != fast.ID {
		t.Fatalf("slow route still won: %s", d.Model.ID)
	}
	slowCand, ok := candidateByModel(d, slow.ID)
	if !ok || slowCand.PerformancePenalty <= 0 {
		t.Fatalf("slow candidate penalty=%v ok=%v", slowCand.PerformancePenalty, ok)
	}
	fastCand, _ := candidateByModel(d, fast.ID)
	if fastCand.PerformancePenalty != 0 {
		t.Fatalf("fast candidate penalty=%v", fastCand.PerformancePenalty)
	}

}

func TestFastVerboseRouteGetsNoThroughputPenalty(t *testing.T) {
	now := time.Now()
	fast := performancePenalty(RoutePerformance{Calls: 100, OutputTokensPerSecond: 179}, now)
	if fast != 0 {
		t.Fatalf("fast verbose route penalty=%v", fast)
	}
	slow := performancePenalty(RoutePerformance{Calls: 100, OutputTokensPerSecond: 9}, now)
	if slow <= 0 {
		t.Fatalf("slow-generating route penalty=%v", slow)
	}
	if performancePenalty(RoutePerformance{Calls: 100, OutputTokensPerSecond: throughputBaselineTokensPerSecond}, now) != 0 {
		t.Fatal("baseline throughput was penalized")
	}
}

func TestFewCallsShrinkPerformancePenalty(t *testing.T) {
	now := time.Now()
	full := performancePenalty(RoutePerformance{Calls: 100, OutputTokensPerSecond: 9, FailureRate: .1}, now)
	shrunk := performancePenalty(RoutePerformance{Calls: 5, OutputTokensPerSecond: 9, FailureRate: .1}, now)
	if full <= 0 || shrunk <= 0 {
		t.Fatalf("full=%v shrunk=%v", full, shrunk)
	}
	weight := func(calls float64) float64 { return calls / (calls + performanceShrinkCalls) }
	want := full * weight(5) / weight(100)
	if math.Abs(shrunk-want) > 1e-9 {
		t.Fatalf("shrunk=%v want %v (full=%v)", shrunk, want, full)
	}
	if shrunk >= full {
		t.Fatalf("few calls must shrink penalty: shrunk=%v full=%v", shrunk, full)
	}
}

func TestRecentSlowCallCoolsOffThenExpires(t *testing.T) {
	slow, _ := model.Get("ramp/claude-sonnet-5-5")
	fast, _ := model.Get("openai/gpt-5.6-terra")
	catalog := []model.ModelSpec{slow, fast}
	model.Install(catalog)
	t.Cleanup(func() { model.Install(model.Catalog) })
	ids := []string{slow.ID, fast.ID}
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	p := &V1{cfg: config.RouterConfig{LambdaCost: .35}, ledger: &ledger{}, catalog: catalog, now: func() time.Time { return now }}
	cool, _, err := p.Decide(context.Background(), RoutingState{
		SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 40_000, AvailableModels: ids,
		Performance: map[string]RoutePerformance{
			// No throughput or reliability hit: only the cool-off should move the score.
			slow.ID: {Calls: 50, OutputTokensPerSecond: 179, LastSlowCall: now.Add(-5 * time.Minute)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cool.Model.ID != fast.ID {
		t.Fatalf("cool-off did not switch away from slow route: %s", cool.Model.ID)
	}
	cand, ok := candidateByModel(cool, slow.ID)
	if !ok || cand.PerformancePenalty != slowCallCoolOffPenalty {
		t.Fatalf("cool-off penalty=%v want %v", cand.PerformancePenalty, slowCallCoolOffPenalty)
	}
	later := now.Add(slowCallCoolOff + time.Minute)
	p.now = func() time.Time { return later }
	after, _, err := p.Decide(context.Background(), RoutingState{
		SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 40_000, AvailableModels: ids,
		Performance: map[string]RoutePerformance{
			slow.ID: {Calls: 50, OutputTokensPerSecond: 179, LastSlowCall: now.Add(-5 * time.Minute)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Model.ID != slow.ID {
		t.Fatalf("after cool-off expected cheaper %s, got %s", slow.ID, after.Model.ID)
	}
	cand, ok = candidateByModel(after, slow.ID)
	if !ok || cand.PerformancePenalty != 0 {
		t.Fatalf("expired cool-off penalty=%v", cand.PerformancePenalty)
	}
}

func TestNoStatsScoresAsBefore(t *testing.T) {
	slow, _ := model.Get("ramp/claude-sonnet-5-5")
	fast, _ := model.Get("openai/gpt-5.6-terra")
	catalog := []model.ModelSpec{slow, fast}
	without := decideWith(t, catalog, RoutingState{Point: TurnStart, Phase: Implement})
	withEmpty := decideWith(t, catalog, RoutingState{Point: TurnStart, Phase: Implement, Performance: map[string]RoutePerformance{}})
	if without.Model.ID != withEmpty.Model.ID {
		t.Fatalf("empty performance map changed choice %s -> %s", without.Model.ID, withEmpty.Model.ID)
	}
	for _, c := range withEmpty.Candidates {
		if c.Rejected == "" && c.PerformancePenalty != 0 {
			t.Fatalf("unexpected penalty on %s: %v", c.Model, c.PerformancePenalty)
		}
	}
}

func TestModelAndEffortPin(t *testing.T) {
	p := NewV1(config.RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"plan"}}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{
		SessionID:       "s",
		Point:           TurnStart,
		Phase:           Plan,
		InputTokens:     1000,
		AvailableModels: []string{"openai/gpt-5.6-sol", "openai/gpt-5.6-terra"},
		ModelPin:        "gpt-5.6-terra",
		EffortPin:       model.EffortLow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "openai/gpt-5.6-terra" {
		t.Fatalf("model pin chose %s", d.Model.ID)
	}
	if d.Effort != model.EffortLow {
		t.Fatalf("effort pin gave %s", d.Effort)
	}
}

func TestEffortLadder(t *testing.T) {
	m := model.ModelSpec{Effort: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortXHigh}}
	cases := []struct {
		name  string
		state RoutingState
		want  model.Effort
	}{
		{"implement default", RoutingState{Phase: Implement}, model.EffortMedium},
		{"long phase does not escalate", RoutingState{Phase: Implement, Stall: StallSignals{PhaseTurns: 30, NoProgressTurns: 9}}, model.EffortMedium},
		{"failed commands escalate", RoutingState{Phase: Implement, Stall: StallSignals{FailedCommands: 2}}, model.EffortHigh},
		{"failing tests escalate", RoutingState{Phase: Explore, Stall: StallSignals{TestFailStreak: 2}}, model.EffortHigh},
		{"review findings escalate", RoutingState{Phase: Implement, Stall: StallSignals{ReviewRejected: true}}, model.EffortHigh},
		{"wrap-up stays light", RoutingState{Phase: WrapUp}, model.EffortLow},
		{"wrap-up failures raise one level", RoutingState{Phase: WrapUp, Stall: StallSignals{RepeatedEdits: 3}}, model.EffortMedium},
		{"review is high and capped", RoutingState{Phase: Review, Stall: StallSignals{FailedCommands: 3}}, model.EffortHigh},
	}
	for _, c := range cases {
		if got := effortFor(m, c.state); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	// An unsupported level falls back to the nearest one below, not the top.
	sparse := model.ModelSpec{Effort: []model.Effort{model.EffortLow, model.EffortXHigh}}
	if got := effortFor(sparse, RoutingState{Phase: Implement}); got != model.EffortLow {
		t.Fatalf("sparse model got %s, want low", got)
	}
}

func TestWorkEffortDefault(t *testing.T) {
	m := model.ModelSpec{WorkEffort: model.EffortLow, Effort: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh}}
	if got := effortFor(m, RoutingState{Phase: Implement}); got != model.EffortLow {
		t.Fatalf("implement got %s, want the model's work effort", got)
	}
	if got := effortFor(m, RoutingState{Phase: Implement, Stall: StallSignals{FailedCommands: 2}}); got != model.EffortMedium {
		t.Fatalf("failures got %s, want one level above the work effort", got)
	}
	if got := effortFor(m, RoutingState{Phase: Plan}); got != model.EffortHigh {
		t.Fatalf("plan got %s, want high", got)
	}
}

func TestTimeValueWeighsStepsAndLatency(t *testing.T) {
	perf := map[string]RoutePerformance{
		"ramp/claude-sonnet-5-5":   {Calls: 200, OutputTokensPerSecond: 300, LatencySeconds: 2, OutputTokensPerCall: 180, CacheReadRatio: .73},
		"ramp/deepseek-v4.1-flash": {Calls: 200, OutputTokensPerSecond: 300, LatencySeconds: 3.1, OutputTokensPerCall: 340, CacheReadRatio: .6},
	}
	decide := func(background bool) string {
		p := NewV1(config.RouterConfig{LambdaCost: .35, TimeValue: config.TimeValue{Interactive: .25}}, &ledger{})
		d, _, err := p.Decide(context.Background(), RoutingState{
			SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 30_000,
			AvailableModels: []string{"ramp/claude-sonnet-5-5", "ramp/deepseek-v4.1-flash"},
			Performance:     perf, Background: background,
		})
		if err != nil {
			t.Fatal(err)
		}
		return d.Model.ID
	}
	if got := decide(false); got != "ramp/claude-sonnet-5-5" {
		t.Fatalf("interactive chose %s; the faster model with fewer steps should win when time is valued", got)
	}
	if got := decide(true); got != "ramp/deepseek-v4.1-flash" {
		t.Fatalf("background chose %s; with time free the cheaper model should win", got)
	}
}

func TestWorkEstimateCountsExtraCalls(t *testing.T) {
	m := model.ModelSpec{ID: "x", CallsPerTask: 1, Pricing: model.Pricing{Input: 1, Output: 1, CacheRead: .1}}
	s := RoutingState{InputTokens: 10_000, Performance: map[string]RoutePerformance{"x": {Calls: 1_000_000, LatencySeconds: 2, OutputTokensPerCall: 100, CacheReadRatio: .5}}}
	one, oneSecs := workEstimate(m, s, 0)
	m.CallsPerTask = 3
	three, threeSecs := workEstimate(m, s, 0)
	if three <= 2*one || threeSecs != 3*oneSecs {
		t.Fatalf("three-call model: cost %.5f vs %.5f, seconds %.1f vs %.1f", three, one, threeSecs, oneSecs)
	}
}

// warmLedger reports a warm cache for the listed models.
type warmLedger struct {
	ledger
	warm map[string]bool
}

func (l *warmLedger) Cache(_ context.Context, _ string, m string) (store.CacheEntry, error) {
	if l.warm[m] {
		return store.CacheEntry{WarmPrefixTokens: 50_000, LastHit: time.Now(), TTL: time.Hour}, nil
	}
	return store.CacheEntry{}, nil
}

func TestStickinessProtectsTheIncumbentNotOtherWarmModels(t *testing.T) {
	candidate := func(d Decision, id string) Candidate {
		for _, c := range d.Candidates {
			if c.Model == id {
				return c
			}
		}
		t.Fatalf("no candidate %s", id)
		return Candidate{}
	}
	state := RoutingState{SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 50_000, ToolContinuation: true, CurrentModel: "ramp/deepseek-v4.1-flash",
		AvailableModels: []string{"ramp/claude-sonnet-5-5", "ramp/deepseek-v4.1-flash"}}
	// The incumbent is cold; Sonnet is warm from earlier in the session. A
	// warm non-incumbent must not be penalised for being warm.
	p := NewV1(config.RouterConfig{LambdaCost: .35}, &warmLedger{warm: map[string]bool{"ramp/claude-sonnet-5-5": true}})
	d, _, err := p.Decide(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if sw := candidate(d, "ramp/claude-sonnet-5-5").SwitchPenalty; sw != 0 {
		t.Fatalf("warm non-incumbent penalised %.2f", sw)
	}
	// A warm incumbent makes leaving it cost a switch penalty.
	p = NewV1(config.RouterConfig{LambdaCost: .35}, &warmLedger{warm: map[string]bool{"ramp/deepseek-v4.1-flash": true}})
	if d, _, err = p.Decide(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if sw := candidate(d, "ramp/claude-sonnet-5-5").SwitchPenalty; sw <= 0 {
		t.Fatal("leaving a warm incumbent must cost a switch penalty")
	}
	if sw := candidate(d, "ramp/deepseek-v4.1-flash").SwitchPenalty; sw != 0 {
		t.Fatalf("the incumbent itself penalised %.2f", sw)
	}
}

func TestUnmeasuredModelsAreNotAssumedEconomical(t *testing.T) {
	s := RoutingState{InputTokens: 10_000}
	measured := model.ModelSpec{ID: "a", CallsPerTask: 1, Pricing: model.Pricing{Input: 1, Output: 1}}
	unknown := model.ModelSpec{ID: "b", Pricing: model.Pricing{Input: 1, Output: 1}}
	_, a := workEstimate(measured, s, 0)
	_, b := workEstimate(unknown, s, 0)
	if b != a*unmeasuredCallsPerTask {
		t.Fatalf("unmeasured seconds %.0f, want %.0f", b, a*unmeasuredCallsPerTask)
	}
}
