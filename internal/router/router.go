package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

type StallSignals struct {
	FailedCommands   int     `json:"failed_commands"`
	RepeatedEdits    int     `json:"repeated_edits"`
	TestFailStreak   int     `json:"test_fail_streak"`
	ToolErrorRate    float64 `json:"tool_error_rate"`
	HumanInterrupt   bool    `json:"human_interrupt"`
	NoProgressTurns  int     `json:"no_progress_turns"`
	PhaseTurns       int     `json:"phase_turns"`
	RepeatedReads    int     `json:"repeated_reads"`
	RepeatedSearches int     `json:"repeated_searches"`
	// ReviewRejected is set while the agent is fixing findings from a failed
	// independent review: correctness work that needs a frontier model.
	ReviewRejected bool `json:"review_rejected,omitempty"`
	// Deescalated drops the warm-cache penalty that keeps an incumbent. It is
	// set once a stall boost has put a frontier model in place and the failure
	// signals have since cleared, so a clean phase can return to efficient.
	Deescalated bool `json:"deescalated,omitempty"`
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
	// OutputTokensPerSecond is the route's recorded generation speed. It is the
	// slowness signal the penalty scores; call latency is not, because it
	// tracks how much a model writes rather than how fast it generates.
	OutputTokensPerSecond float64   `json:"output_tokens_per_second,omitempty"`
	FailureRate           float64   `json:"failure_rate"`
	LastSlowCall          time.Time `json:"last_slow_call,omitzero"`
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
	Model         string       `json:"model"`
	Effort        model.Effort `json:"effort"`
	Quality       float64      `json:"quality"`
	CostUSD       float64      `json:"cost_usd"`
	SwitchPenalty float64      `json:"switch_penalty"`
	// PerformancePenalty is subtracted for recorded slowness and failures.
	PerformancePenalty float64       `json:"performance_penalty"`
	Score              float64       `json:"score"`
	Cache              CacheEstimate `json:"cache"`
	Rejected           string        `json:"rejected,omitempty"`
}
type Decision struct {
	Model          model.ModelSpec   `json:"model"`
	Effort         model.Effort      `json:"effort"`
	EditDialect    model.EditDialect `json:"edit_dialect"`
	ToolsetVariant string            `json:"toolset_variant"`
	WasSwitch      bool              `json:"was_switch"`
	// StallBoost records why the stall boost applied, so its effect is visible
	// in routing.decision instead of only in quality values. Empty means the
	// boost did not apply.
	StallBoost string      `json:"stall_boost"`
	Candidates []Candidate `json:"candidates"`
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
	var candidates []Candidate
	for _, m := range p.catalog {
		c := Candidate{Model: m.ID}
		if s.ModelPin != "" && m.ID != s.ModelPin && m.Model != s.ModelPin {
			c.Rejected = "model pinned"
			candidates = append(candidates, c)
			continue
		}
		if !defaultModelPinned && s.TierPin == "" && s.Point != ReviewCreation && slices.Contains(p.cfg.FrontierFloorPhases, string(s.Phase)) && m.Tier != model.Frontier {
			c.Rejected = "phase has frontier floor"
			candidates = append(candidates, c)
			continue
		}
		if p.cfg.DisableSwitch && p.cfg.DefaultModel != "" && m.ID != p.cfg.DefaultModel {
			c.Rejected = "default model pinned"
			candidates = append(candidates, c)
			continue
		}
		if s.AvailableModels != nil && !slices.Contains(s.AvailableModels, m.ID) {
			c.Rejected = "provider not configured"
			candidates = append(candidates, c)
			continue
		}
		if !defaultModelPinned && s.TierPin == "" && s.Stall.ReviewRejected && (s.Point == TurnStart || s.Point == Escalation) && m.Tier != model.Frontier {
			c.Rejected = "review findings need a frontier model"
			candidates = append(candidates, c)
			continue
		}
		if slices.Contains(s.ExcludeModels, m.ID) {
			c.Rejected = "model excluded after provider failure"
			candidates = append(candidates, c)
			continue
		}
		if s.HasImage && !model.Supports(m, model.Image) {
			c.Rejected = "image unsupported"
			candidates = append(candidates, c)
			continue
		}
		if s.InputTokens+s.EstimatedOutput > m.ContextWindow {
			c.Rejected = "context does not fit"
			candidates = append(candidates, c)
			continue
		}
		if slices.Contains(s.ExcludeFamilies, m.Family) {
			c.Rejected = "family excluded"
			candidates = append(candidates, c)
			continue
		}
		if reviewHasAlternate && m.Family == s.ImplementerFamily {
			c.Rejected = "reviewer must use another family"
			candidates = append(candidates, c)
			continue
		}
		if s.TierPin != "" && m.Tier != s.TierPin {
			c.Rejected = "tier pinned"
			candidates = append(candidates, c)
			continue
		}
		if p.cfg.DisableSwitch && s.CurrentModel != "" && m.ID != s.CurrentModel {
			c.Rejected = "switching disabled"
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
		c.Quality = quality(m.Tier, s.Phase, s.Stall)
		if m.Discovered {
			// A model inferred from a provider listing has no track record
			// here; its tier comes from its price. Built-in and
			// config-vouched models win ties against it.
			c.Quality -= discoveredQualityPenalty
		}
		// Cache stickiness: switching to a warm model costs latency/context; but
		// when the prefix is cold (e.g. right after compaction) there is no cache
		// to preserve, so drop stickiness and let cost/quality decide.
		deescalate := s.Stall.Deescalated && !stalled(s.Stall) && !s.Stall.ReviewRejected && slices.Contains([]Phase{Explore, Implement, WrapUp}, s.Phase)
		if s.ToolContinuation && m.ID != s.CurrentModel && warm && !deescalate {
			c.SwitchPenalty += .18
		}
		// A warm prefix normally keeps the incumbent: rebuilding the cache costs
		// more than a small score gap. That must not hold a frontier model a stall
		// boost put in place once the failures have cleared, or a long clean
		// implement phase never returns to the efficient tier.
		if s.CurrentModel != "" && m.ID != s.CurrentModel && warm && !deescalate {
			c.SwitchPenalty += .08 + math.Min(.25, float64(s.InputTokens)/400000)
		}
		if perf, ok := s.Performance[m.ID]; ok {
			c.PerformancePenalty = performancePenalty(perf, p.now())
		}
		c.Score = c.Quality - p.cfg.LambdaCost*c.CostUSD - c.SwitchPenalty - c.PerformancePenalty
		c.Effort = effortFor(m, s)
		candidates = append(candidates, c)
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
	span.SetAttributes(attribute.String("model", chosen.Model), attribute.Float64("estimated_cost_usd", chosen.CostUSD))
	spec, _ := model.Get(chosen.Model)
	d := Decision{Model: spec, Effort: chosen.Effort, EditDialect: spec.EditDialect, ToolsetVariant: toolset(spec), WasSwitch: s.CurrentModel != "" && s.CurrentModel != spec.ID, StallBoost: stallReason(s.Stall), Candidates: candidates}
	why := explain(s, chosen, d.WasSwitch)
	rec := store.RoutingRecord{ID: uuid.NewString(), SessionID: s.SessionID, Turn: s.Turn, DecisionPoint: string(s.Point), StateJSON: store.JSON(s), CandidatesJSON: store.JSON(candidates), ChosenModel: spec.ID, ChosenEffort: string(chosen.Effort), WasSwitch: d.WasSwitch, CacheEstJSON: store.JSON(chosen.Cache), Explanation: string(why)}
	if err := p.ledger.WriteRouting(ctx, rec); err != nil {
		return Decision{}, "", fmt.Errorf("routing record: %w", err)
	}
	return d, why, nil
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

// Performance penalties keep slow or flaky routes from winning on price alone.
// Throughput and reliability penalties are shrunk toward zero for routes with
// calls (weight = calls/(calls+performanceShrinkCalls)); the cool-off is not.
const (
	performanceShrinkCalls            = 20
	throughputBaselineTokensPerSecond = 40.0
	throughputPenaltyPerToken         = .01
	maxThroughputPenalty              = .3
	maxReliabilityPenalty             = .2
	slowCallCoolOffPenalty            = .2
	slowCallCoolOff                   = 30 * time.Minute
)

// performancePenalty scores a route's recorded generation speed and reliability.
// Routes with no stats get no penalty.
func performancePenalty(perf RoutePerformance, now time.Time) float64 {
	penalty := 0.0
	if perf.Calls > 0 {
		weight := float64(perf.Calls) / float64(perf.Calls+performanceShrinkCalls)
		// Call latency tracks output length more than route speed, so score
		// generation speed: a route that writes long answers fast is not slow.
		// A missing rate means no call reported tokens, so score no throughput
		// penalty rather than an implicit zero.
		throughput := 0.0
		if perf.OutputTokensPerSecond > 0 {
			throughput = math.Min(maxThroughputPenalty, math.Max(0, throughputBaselineTokensPerSecond-perf.OutputTokensPerSecond)*throughputPenaltyPerToken)
		}
		reliability := maxReliabilityPenalty * math.Min(1, math.Max(0, perf.FailureRate))
		penalty += weight * (throughput + reliability)
	}
	if !perf.LastSlowCall.IsZero() && now.Sub(perf.LastSlowCall) < slowCallCoolOff {
		penalty += slowCallCoolOffPenalty
	}
	return penalty
}

// discoveredQualityPenalty offsets the efficient-tier bonus for models known
// only from a provider listing, so price alone cannot make one outscore a
// built-in model on the main loop.
const discoveredQualityPenalty = .25

func quality(t model.Tier, p Phase, stall StallSignals) float64 {
	q := map[model.Tier]float64{model.Frontier: .96, model.Efficient: .78, model.Tiny: .42}[t]
	if slices.Contains([]Phase{Plan, Diagnose, Review}, p) {
		if t == model.Frontier {
			q += .2
		} else {
			q -= .15
		}
	}
	if slices.Contains([]Phase{Explore, Implement, WrapUp}, p) && t == model.Efficient {
		q += .20
	}
	if stalled(stall) {
		readStall := stall.RepeatedReads >= 2 || stall.RepeatedSearches >= 2
		switch {
		case readStall && t == model.Efficient:
			q += .25
		case readStall && t == model.Frontier:
			q += .10
		case readStall:
			q -= .2
		case t == model.Frontier:
			q += .25
		default:
			q -= .2
		}
	}
	return q
}

// stallReason reports the failure signal that escalates quality toward
// frontier. Phase length and turns without an edit are not failures: a long
// implement phase does ordinary work, and treating it as stalled kept frontier
// models in place. Read/search repetition is a stall too, but quality() handles
// it separately so it favours an efficient model.
func stallReason(s StallSignals) string {
	if s.RepeatedReads >= 2 {
		return "repeated reads"
	}
	if s.RepeatedSearches >= 2 {
		return "repeated searches"
	}
	switch {
	case s.FailedCommands >= 2:
		return "failed commands"
	case s.TestFailStreak >= 2:
		return "failing tests"
	case s.RepeatedEdits >= 3:
		return "repeated edits"
	default:
		return ""
	}
}

func stalled(s StallSignals) bool { return stallReason(s) != "" }

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
	if slices.Contains([]Phase{Plan, Diagnose, Review}, s.Phase) {
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
	return Explanation(fmt.Sprintf("%s %s: phase %s, %s, estimated next-call cost $%.4f", action, c.Model, s.Phase, cache, c.CostUSD))
}
func MarshalState(s RoutingState) json.RawMessage { b, _ := json.Marshal(s); return b }
