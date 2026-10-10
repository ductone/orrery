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
	p := NewV1(config.RouterConfig{}, &ledger{})
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
	p := NewV1(config.RouterConfig{}, l)
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
	p := NewV1(config.RouterConfig{}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: ReviewCreation, Phase: Review, InputTokens: 1000, ImplementerFamily: model.OpenAI, AvailableModels: []string{"openai/gpt-5.6-sol", "anthropic/claude-fable-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Family == model.OpenAI {
		t.Fatalf("same-family reviewer: %s", d.Model.ID)
	}
}
func TestReviewFallsBackToImplementerFamilyWhenOnlyProvider(t *testing.T) {
	p := NewV1(config.RouterConfig{}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: ReviewCreation, Phase: Review, InputTokens: 1000, ImplementerFamily: model.XAI, AvailableModels: []string{"xai/grok-4.6"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.ID != "xai/grok-4.6" {
		t.Fatalf("reviewer=%s", d.Model.ID)
	}
}
func TestReviewCrossFamilyOverridesFrontierFloor(t *testing.T) {
	p := NewV1(config.RouterConfig{FrontierFloorPhases: []string{"review"}}, &ledger{})
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
	p := NewV1(config.RouterConfig{FrontierFloorPhases: []string{"plan"}}, l)
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

func TestImplementRoutesEfficientAtDefaultWeight(t *testing.T) {
	l := &ledger{}
	p := NewV1(config.RouterConfig{}, l)
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: JobCreation, Phase: Implement, InputTokens: 1000, EstimatedOutput: 4000, AvailableModels: []string{"openai/gpt-5.6-sol", "together/deepseek-ai/DeepSeek-V4-Flash-0731"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model.Tier != model.Efficient {
		t.Fatalf("implement chose %s", d.Model.ID)
	}
}

// With a cold prefix (post-compaction) there is no warm cache to preserve, so
// stickiness must not keep an expensive incumbent ahead of a cheaper fit.
func TestColdPrefixDropsStickiness(t *testing.T) {
	l := &ledger{} // empty cache ledger => all cold
	p := NewV1(config.RouterConfig{}, l)
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
	p := &V1{cfg: config.RouterConfig{}, ledger: &ledger{}, catalog: catalog, now: time.Now}
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

func TestModelAndEffortPin(t *testing.T) {
	p := NewV1(config.RouterConfig{FrontierFloorPhases: []string{"plan"}}, &ledger{})
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
		"ramp/claude-sonnet-5-5":   {Calls: 200, LatencySeconds: 2, OutputTokensPerCall: 180, CacheReadRatio: .73},
		"ramp/deepseek-v4.1-flash": {Calls: 200, LatencySeconds: 3.1, OutputTokensPerCall: 340, CacheReadRatio: .6},
	}
	decide := func(background bool) string {
		p := NewV1(config.RouterConfig{TimeValue: config.TimeValue{Interactive: .25}}, &ledger{})
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
	one, oneSecs := workEstimate(m, s, 0, "")
	m.CallsPerTask = 3
	three, threeSecs := workEstimate(m, s, 0, "")
	if three <= 2*one || math.Abs(threeSecs-3*oneSecs) > .01 {
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

// Replays session 1c4e2298 seq 61: a frontier model chosen at plan, warm on a
// 37K prefix, must not be held through implement by a switch penalty when a
// cheaper, faster efficient model repays its cold first call within the work
// horizon. A mid-chain switch still needs only the small margin.
func TestWarmFrontierIncumbentLosesToEfficientModelInImplement(t *testing.T) {
	// Default router config: time is valued, as in the live session.
	p := NewV1(config.RouterConfig{TimeValue: config.TimeValue{Interactive: .25}}, &warmLedger{warm: map[string]bool{"ramp/claude-opus-5-5": true}})
	for _, chain := range []bool{false, true} {
		state := RoutingState{SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 37_000, ToolContinuation: chain, CurrentModel: "ramp/claude-opus-5-5",
			AvailableModels: []string{"ramp/claude-opus-5-5", "ramp/claude-sonnet-5-5"},
			// Recorded latencies give seq 61's ~43s (Opus) vs ~17s (Sonnet) of work.
			Performance: map[string]RoutePerformance{
				"ramp/claude-opus-5-5":   {Calls: 1000, LatencySeconds: 10.75},
				"ramp/claude-sonnet-5-5": {Calls: 1000, LatencySeconds: 4.25},
			}}
		d, _, err := p.Decide(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		if d.Model.ID != "ramp/claude-sonnet-5-5" {
			t.Fatalf("tool continuation=%v: kept %s, want sonnet: %+v", chain, d.Model.ID, d.Candidates)
		}
	}
}

func TestUnmeasuredModelsAreNotAssumedEconomical(t *testing.T) {
	s := RoutingState{InputTokens: 10_000}
	measured := model.ModelSpec{ID: "a", CallsPerTask: 1, Pricing: model.Pricing{Input: 1, Output: 1}}
	unknown := model.ModelSpec{ID: "b", Pricing: model.Pricing{Input: 1, Output: 1}}
	_, a := workEstimate(measured, s, 0, "")
	_, b := workEstimate(unknown, s, 0, "")
	if b != a*unmeasuredCallsPerTask {
		t.Fatalf("unmeasured seconds %.0f, want %.0f", b, a*unmeasuredCallsPerTask)
	}
}

func TestTinyModelsAreALastResort(t *testing.T) {
	p := NewV1(config.RouterConfig{}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 1000,
		AvailableModels: []string{"openai/gpt-5.6-luna", "openai/gpt-5.6-terra"}})
	if err != nil || d.Model.ID != "openai/gpt-5.6-terra" {
		t.Fatalf("chose %s err=%v; a tiny model must not win while another is eligible", d.Model.ID, err)
	}
	d, _, err = p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Implement, InputTokens: 1000,
		AvailableModels: []string{"openai/gpt-5.6-luna"}})
	if err != nil || d.Model.ID != "openai/gpt-5.6-luna" {
		t.Fatalf("chose %s err=%v; a tiny model is used when nothing else is left", d.Model.ID, err)
	}
}

func TestFrontierFloorYieldsWhenNothingElseIsLeft(t *testing.T) {
	p := NewV1(config.RouterConfig{FrontierFloorPhases: []string{"plan"}}, &ledger{})
	d, _, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Point: TurnStart, Phase: Plan, InputTokens: 1000,
		AvailableModels: []string{"openai/gpt-5.6-terra"}})
	if err != nil || d.Model.ID != "openai/gpt-5.6-terra" {
		t.Fatalf("chose %s err=%v", d.Model.ID, err)
	}
}

func TestFailuresArePricedAsRetries(t *testing.T) {
	m := model.ModelSpec{ID: "x", CallsPerTask: 1, Pricing: model.Pricing{Input: 1, Output: 1}}
	clean := RoutingState{InputTokens: 10_000, Performance: map[string]RoutePerformance{"x": {Calls: 1_000_000, LatencySeconds: 2}}}
	flaky := RoutingState{InputTokens: 10_000, Performance: map[string]RoutePerformance{"x": {Calls: 1_000_000, LatencySeconds: 2, FailureRate: .2}}}
	c1, s1 := workEstimate(m, clean, 0, "")
	c2, s2 := workEstimate(m, flaky, 0, "")
	if c2 <= c1*1.2 || s2 <= s1*1.2 {
		t.Fatalf("a 20%% failure rate must cost ~25%% more: cost %.5f vs %.5f, seconds %.1f vs %.1f", c2, c1, s2, s1)
	}
}

func TestWorkEstimateUsesTheEffortsOwnStats(t *testing.T) {
	m := model.ModelSpec{ID: "x", CallsPerTask: 1, Pricing: model.Pricing{Input: 1, Output: 1}}
	s := RoutingState{InputTokens: 10_000, Performance: map[string]RoutePerformance{
		"x":                                  {Calls: 1_000_000, LatencySeconds: 30, OutputTokensPerCall: 3000},
		EffortStatsKey("x", model.EffortLow): {Calls: 1_000_000, LatencySeconds: 2, OutputTokensPerCall: 200},
	}}
	_, low := workEstimate(m, s, 0, model.EffortLow)
	_, high := workEstimate(m, s, 0, model.EffortHigh)
	if math.Abs(low-2*workHorizonCalls) > .1 || math.Abs(high-30*workHorizonCalls) > .1 {
		t.Fatalf("low %.0fs, high %.0fs: a call at low effort must use the low-effort latency", low, high)
	}
}

func TestBackgroundWorkExploresUnmeasuredModels(t *testing.T) {
	p := NewV1(config.RouterConfig{}, &ledger{})
	measured := map[string]RoutePerformance{"ramp/claude-sonnet-5-5": {Calls: 1000, LatencySeconds: 2}}
	explored, exploited := 0, 0
	for turn := range 200 {
		for _, background := range []bool{false, true} {
			d, why, err := p.Decide(context.Background(), RoutingState{SessionID: "s", Turn: turn, Point: TurnStart, Phase: Implement, InputTokens: 1000, Background: background,
				AvailableModels: []string{"ramp/claude-sonnet-5-5", "ramp/deepseek-v4.1-flash"}, Performance: measured})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(string(why), "exploring") {
				if !background || d.Model.ID != "ramp/deepseek-v4.1-flash" {
					t.Fatalf("explored %s in background=%v", d.Model.ID, background)
				}
				explored++
			} else if background {
				exploited++
			}
		}
	}
	if explored == 0 || explored > 40 {
		t.Fatalf("explored %d of 200 background decisions; want about one in %d", explored, exploreEvery)
	}
}
