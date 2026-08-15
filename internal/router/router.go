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
	FailedCommands int     `json:"failed_commands"`
	RepeatedEdits  int     `json:"repeated_edits"`
	TestFailStreak int     `json:"test_fail_streak"`
	ToolErrorRate  float64 `json:"tool_error_rate"`
	HumanInterrupt bool    `json:"human_interrupt"`
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
	SessionID         string         `json:"session_id"`
	Turn              int            `json:"turn"`
	Point             DecisionPoint  `json:"decision_point"`
	Phase             Phase          `json:"phase"`
	CurrentModel      string         `json:"current_model,omitempty"`
	InputTokens       int            `json:"input_tokens"`
	EstimatedOutput   int            `json:"estimated_output"`
	HasImage          bool           `json:"has_image,omitempty"`
	ToolContinuation  bool           `json:"tool_continuation,omitempty"`
	NewInstruction    bool           `json:"new_instruction,omitempty"`
	Stall             StallSignals   `json:"stall"`
	ExcludeFamilies   []model.Family `json:"exclude_families,omitempty"`
	ExcludeModels     []string       `json:"exclude_models,omitempty"`
	AvailableModels   []string       `json:"available_models,omitempty"`
	TierPin           model.Tier     `json:"tier_pin,omitempty"`
	ImplementerFamily model.Family   `json:"implementer_family,omitempty"`
}
type Candidate struct {
	Model         string        `json:"model"`
	Effort        model.Effort  `json:"effort"`
	Quality       float64       `json:"quality"`
	CostUSD       float64       `json:"cost_usd"`
	SwitchPenalty float64       `json:"switch_penalty"`
	Score         float64       `json:"score"`
	Cache         CacheEstimate `json:"cache"`
	Rejected      string        `json:"rejected,omitempty"`
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
	return &V1{cfg: cfg, ledger: l, catalog: model.Catalog, now: time.Now}
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
	var candidates []Candidate
	for _, m := range p.catalog {
		c := Candidate{Model: m.ID}
		if s.TierPin == "" && slices.Contains(p.cfg.FrontierFloorPhases, string(s.Phase)) && m.Tier != model.Frontier {
			c.Rejected = "phase has frontier floor"
			candidates = append(candidates, c)
			continue
		}
		if p.cfg.DisableSwitch && p.cfg.DefaultModel != "" && m.ID != p.cfg.DefaultModel {
			c.Rejected = "default model pinned"
			candidates = append(candidates, c)
			continue
		}
		if len(s.AvailableModels) > 0 && !slices.Contains(s.AvailableModels, m.ID) {
			c.Rejected = "provider not configured"
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
		if s.Point == ReviewCreation && s.ImplementerFamily != "" && m.Family == s.ImplementerFamily {
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
		if s.ToolContinuation && m.ID != s.CurrentModel {
			c.SwitchPenalty += .18
		}
		if s.CurrentModel != "" && m.ID != s.CurrentModel {
			c.SwitchPenalty += .08 + math.Min(.25, float64(s.InputTokens)/400000)
		}
		c.Score = c.Quality - p.cfg.LambdaCost*c.CostUSD - c.SwitchPenalty
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
		return strings.Compare(a.Model, b.Model)
	})
	chosen := valid[0]
	span.SetAttributes(attribute.String("model", chosen.Model), attribute.Float64("estimated_cost_usd", chosen.CostUSD))
	spec, _ := model.Get(chosen.Model)
	d := Decision{Model: spec, Effort: chosen.Effort, EditDialect: spec.EditDialect, ToolsetVariant: toolset(spec), WasSwitch: s.CurrentModel != "" && s.CurrentModel != spec.ID, Candidates: candidates}
	why := explain(s, chosen, d.WasSwitch)
	rec := store.RoutingRecord{ID: uuid.NewString(), SessionID: s.SessionID, Turn: s.Turn, DecisionPoint: string(s.Point), StateJSON: store.JSON(s), CandidatesJSON: store.JSON(candidates), ChosenModel: spec.ID, ChosenEffort: string(chosen.Effort), WasSwitch: d.WasSwitch, CacheEstJSON: store.JSON(chosen.Cache), Explanation: string(why)}
	if err := p.ledger.WriteRouting(ctx, rec); err != nil {
		return Decision{}, "", fmt.Errorf("routing record: %w", err)
	}
	return d, why, nil
}
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
	if stall.FailedCommands >= 2 || stall.TestFailStreak >= 2 || stall.RepeatedEdits >= 3 {
		if t == model.Frontier {
			q += .25
		} else {
			q -= .2
		}
	}
	return q
}
func effortFor(m model.ModelSpec, s RoutingState) model.Effort {
	want := model.EffortMedium
	if slices.Contains([]Phase{Plan, Diagnose, Review}, s.Phase) || s.Stall.TestFailStreak >= 2 {
		want = model.EffortHigh
	}
	if s.Phase == WrapUp {
		want = model.EffortLow
	}
	if slices.Contains(m.Effort, want) {
		return want
	}
	if len(m.Effort) > 0 {
		return m.Effort[len(m.Effort)-1]
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
