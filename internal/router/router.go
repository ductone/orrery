package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
	"github.com/google/uuid"
)

type DecisionPoint string

const (
	TurnStart      DecisionPoint = "turn"
	JobCreation    DecisionPoint = "spawn"
	ReviewCreation DecisionPoint = "review"
	Escalation     DecisionPoint = "escalation"
)

type Phase string

const (
	Explore   Phase = "explore"
	Plan      Phase = "plan"
	Implement Phase = "implement"
	Diagnose  Phase = "diagnose"
	Review    Phase = "review"
	WrapUp    Phase = "wrap-up"
)

// StallSignals are the failure signals that raise reasoning effort, and the
// review state that requires a frontier model.
type StallSignals struct {
	FailedCommands int `json:"failed_commands"`
	RepeatedEdits  int `json:"repeated_edits"`
	TestFailStreak int `json:"test_fail_streak"`
	// ReviewRejected is set while the agent is fixing findings from a failed
	// independent review: correctness work that needs a frontier model.
	ReviewRejected bool `json:"review_rejected,omitempty"`
}
type CacheEstimate struct {
	WarmTokens      int     `json:"warm_tokens,omitempty"`
	FreshTokens     int     `json:"fresh_tokens,omitempty"`
	EstimatedOutput int     `json:"estimated_output,omitempty"`
	Warm            bool    `json:"warm"`
	CostUSD         float64 `json:"cost_usd"`
	TokensToCliff   int     `json:"tokens_to_cliff,omitempty"`
}
type RoutingState struct {
	SessionID        string        `json:"session_id"`
	Turn             int           `json:"turn"`
	Point            DecisionPoint `json:"decision_point"`
	Phase            Phase         `json:"phase"`
	CurrentModel     string        `json:"current_model,omitempty"`
	InputTokens      int           `json:"input_tokens"`
	EstimatedOutput  int           `json:"estimated_output"`
	HasImage         bool          `json:"has_image,omitempty"`
	ToolContinuation bool          `json:"tool_continuation,omitempty"`
	NewInstruction   bool          `json:"new_instruction,omitempty"`
	// InstructionPhase records how a new instruction's phase was chosen:
	// "plan" by default, or a classifier's answer and its confidence.
	InstructionPhase *InstructionPhase `json:"instruction_phase,omitempty"`
	Stall            StallSignals      `json:"stall"`
	ExcludeFamilies  []model.Family    `json:"exclude_families,omitempty"`
	ExcludeModels    []string          `json:"exclude_models,omitempty"`
	AvailableModels  []string          `json:"available_models,omitempty"`
	TierPin          model.Tier        `json:"tier_pin,omitempty"`
	// Background marks work nobody is waiting on, so time is valued at the
	// background rate instead of the interactive one.
	Background bool `json:"background,omitempty"`
	// ModelPin restricts the decision to one route (id or canonical model
	// name) and EffortPin fixes its effort when the model supports it.
	ModelPin          string       `json:"model_pin,omitempty"`
	EffortPin         model.Effort `json:"effort_pin,omitempty"`
	ImplementerFamily model.Family `json:"implementer_family,omitempty"`
	// Performance is recorded per-route latency and reliability, keyed by
	// route id. Routes absent from the map are scored without a penalty.
	Performance map[string]RoutePerformance `json:"performance,omitempty"`
}

// RoutePerformance summarizes a route's recorded calls for scoring.
type RoutePerformance struct {
	Calls int `json:"calls"`
	// FailureRate is the share of calls that failed and had to be retried
	// (empty, malformed, provider errors); routing prices it as retries.
	FailureRate float64 `json:"failure_rate"`
	// LatencySeconds, OutputTokensPerCall and CacheReadRatio are the route's
	// typical call, for estimating what a unit of work costs on it.
	LatencySeconds      float64 `json:"latency_seconds,omitempty"`
	OutputTokensPerCall float64 `json:"output_tokens_per_call,omitempty"`
	CacheReadRatio      float64 `json:"cache_read_ratio,omitempty"`
}

// InstructionPhase is the phase chosen for a turn that starts with a new user
// message, and where the choice came from.
type InstructionPhase struct {
	QuestionVersion string  `json:"question_version,omitempty"`
	Phase           Phase   `json:"phase"`
	Source          string  `json:"source"`
	Confidence      float64 `json:"confidence,omitempty"`
	Suggested       Phase   `json:"suggested,omitempty"`
	Error           string  `json:"error,omitempty"`
}

type Candidate struct {
	Model   string       `json:"model"`
	Effort  model.Effort `json:"effort"`
	CostUSD float64      `json:"cost_usd"`
	// WorkCostUSD and WorkSeconds estimate a unit of work on this model: the
	// next call plus the extra calls it typically needs (CallsPerTask), at its
	// typical output and cache reuse. TimeCostUSD values WorkSeconds at the
	// interactive or background rate.
	WorkCostUSD float64 `json:"work_cost_usd"`
	WorkSeconds float64 `json:"work_seconds"`
	TimeCostUSD float64 `json:"time_cost_usd"`
	// Score is minus the candidate's total cost in dollars, work and time.
	// The highest score wins.
	Score    float64       `json:"score"`
	Cache    CacheEstimate `json:"cache"`
	Rejected string        `json:"rejected,omitempty"`
}
type Decision struct {
	Model          model.ModelSpec   `json:"model"`
	Effort         model.Effort      `json:"effort"`
	EditDialect    model.EditDialect `json:"edit_dialect"`
	ToolsetVariant string            `json:"toolset_variant"`
	WasSwitch      bool              `json:"was_switch"`
	Candidates     []Candidate       `json:"candidates"`
}
type Explanation string
type Policy interface {
	Decide(context.Context, RoutingState) (Decision, Explanation, error)
}

type Ledger interface {
	Cache(context.Context, string, string) (store.CacheEntry, error)
	WriteRouting(context.Context, store.RoutingRecord) error
}
type V1 struct {
	cfg     config.RouterConfig
	ledger  Ledger
	catalog []model.ModelSpec
	now     func() time.Time
}

func NewV1(cfg config.RouterConfig, l Ledger) *V1 {
	return &V1{cfg: cfg, ledger: l, catalog: model.All(), now: time.Now}
}

func (p *V1) Decide(ctx context.Context, s RoutingState) (Decision, Explanation, error) {
	ctx, span := otel.Tracer("orrery/router").Start(ctx, "routing.decision")
	defer span.End()
	span.SetAttributes(attribute.String("decision.point", string(s.Point)), attribute.String("phase", string(s.Phase)))
	if s.InputTokens <= 0 {
		s.InputTokens = 4000
	}
	if s.EstimatedOutput <= 0 {
		s.EstimatedOutput = 2000
	}
	reviewHasAlternate := p.reviewHasAlternateFamily(s)
	defaultModelPinned := (p.cfg.DisableSwitch && p.cfg.DefaultModel != "") || s.ModelPin != ""
	candidates := p.score(ctx, s, reviewHasAlternate, defaultModelPinned, false)
	if !anyValid(candidates) {
		// A floor (frontier for judgement phases, no tiny models) yields when
		// nothing else is left rather than failing the turn.
		candidates = p.score(ctx, s, reviewHasAlternate, defaultModelPinned, true)
	}
	valid := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Rejected == "" {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return Decision{}, "", errors.New("router: no compatible models")
	}
	slices.SortFunc(valid, func(a, b Candidate) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		if a.Model == s.CurrentModel {
			return -1
		}
		if s.CurrentModel == "" && p.cfg.DefaultModel != "" {
			if a.Model == p.cfg.DefaultModel {
				return -1
			}
			if b.Model == p.cfg.DefaultModel {
				return 1
			}
		}
		return strings.Compare(a.Model, b.Model)
	})
	chosen := valid[0]
	explored := false
	if pick, ok := p.exploration(s, valid); ok {
		chosen, explored = pick, true
	}
	span.SetAttributes(attribute.String("model", chosen.Model), attribute.Float64("estimated_cost_usd", chosen.CostUSD))
	spec, _ := model.Get(chosen.Model)
	d := Decision{Model: spec, Effort: chosen.Effort, EditDialect: spec.EditDialect, ToolsetVariant: toolset(spec), WasSwitch: s.CurrentModel != "" && s.CurrentModel != spec.ID, Candidates: candidates}
	why := explain(s, chosen, d.WasSwitch)
	if explored {
		why = "exploring: " + why
	}
	rec := store.RoutingRecord{ID: uuid.NewString(), SessionID: s.SessionID, Turn: s.Turn, DecisionPoint: string(s.Point), StateJSON: store.JSON(s), CandidatesJSON: store.JSON(candidates), ChosenModel: spec.ID, ChosenEffort: string(chosen.Effort), WasSwitch: d.WasSwitch, CacheEstJSON: store.JSON(chosen.Cache), Explanation: string(why)}
	if err := p.ledger.WriteRouting(ctx, rec); err != nil {
		return Decision{}, "", fmt.Errorf("routing record: %w", err)
	}
	return d, why, nil
}

// score evaluates every catalog model for s. Eligibility is a set of hard
// filters plus two floors: judgement phases (router.frontier_floor_phases, and
// fixing review findings) need a frontier model, and tiny models are a last
// resort. relaxFloors drops the floors when nothing else is eligible. Eligible
// models are then ranked purely by cost: the dollars a unit of work costs,
// the value of the time it takes, and a small margin for switching mid tool
// chain.
func (p *V1) score(ctx context.Context, s RoutingState, reviewHasAlternate, defaultModelPinned, relaxFloors bool) []Candidate {
	floors := !relaxFloors && !defaultModelPinned && s.TierPin == ""
	frontierFloor := floors && (slices.Contains(p.cfg.FrontierFloorPhases, string(s.Phase)) || s.Point == ReviewCreation ||
		(s.Stall.ReviewRejected && (s.Point == TurnStart || s.Point == Escalation)))
	var candidates []Candidate
	for _, m := range p.catalog {
		c := Candidate{Model: m.ID}
		switch {
		case s.ModelPin != "" && m.ID != s.ModelPin && m.Model != s.ModelPin:
			c.Rejected = "model pinned"
		case p.cfg.DisableSwitch && p.cfg.DefaultModel != "" && m.ID != p.cfg.DefaultModel:
			c.Rejected = "default model pinned"
		case s.AvailableModels != nil && !slices.Contains(s.AvailableModels, m.ID):
			c.Rejected = "provider not configured"
		case frontierFloor && m.Tier != model.Frontier:
			c.Rejected = "needs a frontier model"
		case floors && m.Tier == model.Tiny:
			c.Rejected = "tiny models are a last resort"
		case slices.Contains(s.ExcludeModels, m.ID):
			c.Rejected = "model excluded after provider failure"
		case s.HasImage && !model.Supports(m, model.Image):
			c.Rejected = "image unsupported"
		case s.InputTokens+s.EstimatedOutput > m.ContextWindow:
			c.Rejected = "context does not fit"
		case slices.Contains(s.ExcludeFamilies, m.Family):
			c.Rejected = "family excluded"
		case reviewHasAlternate && m.Family == s.ImplementerFamily:
			c.Rejected = "reviewer must use another family"
		case s.TierPin != "" && m.Tier != s.TierPin:
			c.Rejected = "tier pinned"
		case p.cfg.DisableSwitch && s.CurrentModel != "" && m.ID != s.CurrentModel:
			c.Rejected = "switching disabled"
		}
		if c.Rejected != "" {
			candidates = append(candidates, c)
			continue
		}
		entry, _ := p.ledger.Cache(ctx, s.SessionID, m.ID)
		warm := entry.Valid(p.now())
		warmTokens := 0
		if warm {
			warmTokens = min(entry.WarmPrefixTokens, s.InputTokens)
		}
		c.Cache = CacheEstimate{WarmTokens: warmTokens, FreshTokens: s.InputTokens - warmTokens, Warm: warm, CostUSD: m.Pricing.Estimate(s.InputTokens, s.EstimatedOutput, warmTokens)}
		c.CostUSD = c.Cache.CostUSD
		for _, tr := range m.Pricing.Thresholds {
			if tr.AboveTokens > s.InputTokens {
				c.Cache.TokensToCliff = tr.AboveTokens - s.InputTokens
				break
			}
		}
		c.Effort = effortFor(m, s)
		c.WorkCostUSD, c.WorkSeconds = workEstimate(m, s, warmTokens, c.Effort)
		c.TimeCostUSD = c.WorkSeconds / 60 * p.cfg.TimeValue.For(s.Background)
		// Each candidate's work cost prices its next call at its actual cache
		// warmth, so switching away from a warm model is already paid for.
		c.Score = -(c.WorkCostUSD + c.TimeCostUSD)
		candidates = append(candidates, c)
	}
	return candidates
}

// EffortStatsKey is the model_stats key for a route's calls at one effort.
func EffortStatsKey(route string, effort model.Effort) string {
	return route + "@" + string(effort)
}

// exploration picks a route to measure instead of the best-scoring one: in
// background work (time is not valued), on one decision in exploreEvery
// (chosen deterministically from the session and turn so replays agree), the
// cheapest eligible route that has recorded fewer than exploreCalls calls.
func (p *V1) exploration(s RoutingState, valid []Candidate) (Candidate, bool) {
	if !s.Background || s.ModelPin != "" || s.Point == ReviewCreation {
		return Candidate{}, false
	}
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s/%d/%s", s.SessionID, s.Turn, s.Point)
	if h.Sum32()%exploreEvery != 0 {
		return Candidate{}, false
	}
	var pick Candidate
	found := false
	for _, c := range valid {
		if s.Performance[c.Model].Calls >= exploreCalls {
			continue
		}
		if !found || c.WorkCostUSD < pick.WorkCostUSD {
			pick, found = c, true
		}
	}
	return pick, found
}

func anyValid(cs []Candidate) bool {
	for _, c := range cs {
		if c.Rejected == "" {
			return true
		}
	}
	return false
}

// reviewHasAlternateFamily keeps cross-family review as the default without
// making review impossible in a single-provider deployment. It mirrors the
// hard compatibility filters used by Decide; scoring still chooses the final
// reviewer once an alternative is known to exist.
func (p *V1) reviewHasAlternateFamily(s RoutingState) bool {
	if s.Point != ReviewCreation || s.ImplementerFamily == "" {
		return false
	}
	for _, m := range p.catalog {
		if m.Family == s.ImplementerFamily ||
			(s.AvailableModels != nil && !slices.Contains(s.AvailableModels, m.ID)) ||
			slices.Contains(s.ExcludeModels, m.ID) ||
			slices.Contains(s.ExcludeFamilies, m.Family) ||
			(s.HasImage && !model.Supports(m, model.Image)) ||
			s.InputTokens+s.EstimatedOutput > m.ContextWindow ||
			(s.TierPin != "" && m.Tier != s.TierPin) ||
			(p.cfg.DisableSwitch && p.cfg.DefaultModel != "" && m.ID != p.cfg.DefaultModel) {
			continue
		}
		return true
	}
	return false
}

// Priors for a route's typical call, used in proportion to how few calls it
// has recorded (weight = calls/(calls+performanceShrinkCalls)).
const (
	performanceShrinkCalls = 20
	// exploreEvery and exploreCalls: one background decision in exploreEvery
	// goes to the cheapest eligible route with fewer than exploreCalls recorded
	// calls, so unmeasured models build a history where nobody waits for it.
	exploreEvery        = 10
	exploreCalls        = 20
	outputTokensPrior   = 1000
	cacheReadRatioPrior = .7
	latencySecondsPrior = 10
	// workHorizonCalls is the unit of work routing prices, in calls of a
	// reference model. A switch's cold first call is amortised over it, so it
	// must be about as long as the work that follows a decision: with four
	// calls a warm frontier model looked cheaper than a cold efficient one
	// that costs half as much per task.
	workHorizonCalls = 10
	// unmeasuredCallsPerTask is assumed for a model no sweep has measured. The
	// measured models other than Claude took 2-2.7 times Claude's calls, so an
	// unknown model is not assumed to be as economical as the best one.
	unmeasuredCallsPerTask = 2
)

// workEstimate is the cost and time of a unit of work (workHorizonCalls
// reference calls) on m: the next call at the session's actual cache warmth,
// then the rest of the calls m typically takes for that work, each at its
// typical output and cache reuse. A model that needs more steps pays for
// re-sending the context each time.
func workEstimate(m model.ModelSpec, s RoutingState, warmTokens int, effort model.Effort) (float64, float64) {
	perf := s.Performance[m.ID]
	weight := float64(perf.Calls) / float64(perf.Calls+performanceShrinkCalls)
	blend := func(observed, prior float64, w float64) float64 {
		if observed <= 0 {
			return prior
		}
		return w*observed + (1-w)*prior
	}
	// The route's own figures first, shrunk toward priors; then the figures
	// for the effort the call would run at, shrunk toward the route's.
	output := blend(perf.OutputTokensPerCall, outputTokensPrior, weight)
	cacheRatio := blend(perf.CacheReadRatio, cacheReadRatioPrior, weight)
	latency := blend(perf.LatencySeconds, latencySecondsPrior, weight)
	if at, ok := s.Performance[EffortStatsKey(m.ID, effort)]; ok && effort != "" {
		w := float64(at.Calls) / float64(at.Calls+performanceShrinkCalls)
		output = blend(at.OutputTokensPerCall, output, w)
		cacheRatio = blend(at.CacheReadRatio, cacheRatio, w)
		latency = blend(at.LatencySeconds, latency, w)
	}
	calls := m.CallsPerTask
	if calls <= 0 {
		calls = unmeasuredCallsPerTask
	}
	next := m.Pricing.Estimate(s.InputTokens, int(output), warmTokens)
	steady := m.Pricing.Estimate(s.InputTokens, int(output), int(cacheRatio*float64(s.InputTokens)))
	total := workHorizonCalls * calls
	// A failed call is retried: price the expected retries.
	retries := 1 / (1 - math.Min(.5, weight*perf.FailureRate))
	return (next + (total-1)*steady) * retries, total * latency * retries
}

// effortLadder orders reasoning levels from least to most.
var effortLadder = []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortXHigh}

// effortFor picks a call's reasoning effort: a phase default, raised one
// level while real failures persist (failed commands, failing tests, repeated
// edits, or review findings to fix). Phase length never raises it. The
// failure signals clear only when the agent makes progress, so the level
// changes rarely; that matters because an effort change can cost the prompt
// cache on some providers.
func effortFor(m model.ModelSpec, s RoutingState) model.Effort {
	if s.EffortPin != "" && slices.Contains(m.Effort, s.EffortPin) {
		return s.EffortPin
	}
	want := model.EffortMedium
	// The agent checking its own work is ordinary work: an independent
	// reviewer checks it again, at high effort.
	if m.WorkEffort != "" && (slices.Contains([]Phase{Explore, Implement}, s.Phase) || s.Phase == Review && s.Point != ReviewCreation) {
		want = m.WorkEffort
	}
	if s.Phase == Plan {
		// Planning starts at the model's own default; failures raise it.
		want = model.EffortMedium
	}
	if s.Phase == Diagnose || s.Point == ReviewCreation {
		want = model.EffortHigh
	}
	if s.Phase == WrapUp {
		want = model.EffortLow
	}
	if failing(s.Stall) && want != model.EffortHigh {
		want = effortLadder[slices.Index(effortLadder, want)+1]
	}
	return supportedEffort(m, want)
}

// failing reports failure signals that justify more reasoning.
func failing(s StallSignals) bool {
	return s.FailedCommands >= 2 || s.TestFailStreak >= 2 || s.RepeatedEdits >= 3 || s.ReviewRejected
}

// supportedEffort returns want if m supports it, else the nearest supported
// level below it, else the nearest above it.
func supportedEffort(m model.ModelSpec, want model.Effort) model.Effort {
	at := slices.Index(effortLadder, want)
	for i := at; i >= 0; i-- {
		if slices.Contains(m.Effort, effortLadder[i]) {
			return effortLadder[i]
		}
	}
	for i := at + 1; i < len(effortLadder); i++ {
		if slices.Contains(m.Effort, effortLadder[i]) {
			return effortLadder[i]
		}
	}
	return model.EffortNone
}
func toolset(m model.ModelSpec) string {
	if m.Compat.SupportsStrictTools {
		return "strict"
	}
	return "portable"
}
func explain(s RoutingState, c Candidate, sw bool) Explanation {
	action := "stayed on"
	if sw || s.CurrentModel == "" {
		action = "selected"
	}
	cache := "cold prefix"
	if c.Cache.Warm {
		cache = fmt.Sprintf("warm prefix %dK", c.Cache.WarmTokens/1000)
	}
	return Explanation(fmt.Sprintf("%s %s: phase %s, %s, estimated next-call cost $%.4f, work $%.4f over %.0fs", action, c.Model, s.Phase, cache, c.CostUSD, c.WorkCostUSD, c.WorkSeconds))
}
func MarshalState(s RoutingState) json.RawMessage { b, _ := json.Marshal(s); return b }
