package model

import (
	"strings"
	"time"
)

// Model is a model as its lab ships it, whoever serves it. Tier is Orrery's
// judgement of the model's quality, so it holds on every route; the facts that
// differ by provider (wire id, price, API quirks) belong to a Route.
type Model struct {
	// Name is the canonical id, the lab's own model id.
	Name   string
	Family Family
	Tier   Tier
	Inputs []Modality
	// ContextWindow and MaxOutput are the limits as served; a route that
	// serves less overrides them.
	ContextWindow, MaxOutput int
	// Effort lists the reasoning levels the model supports, EffortNone
	// included when it can answer without reasoning.
	Effort []Effort
	// WorkEffort is the reasoning level for routine explore and implement
	// turns, from the 2026-10-09 effort sweep; empty means medium.
	WorkEffort Effort
	// CallsPerTask is how many calls the model typically takes for the work a
	// reference model (Claude Opus/Sonnet 5.5) does in one, from the same
	// sweep; zero means 1. Routing multiplies per-call cost and time by it.
	CallsPerTask      float64
	EditDialect       EditDialect
	StreamIdleTimeout time.Duration
	// FirstByteTimeout is how long to wait for a response's first byte
	// before treating the call as a stalled transport failure. Zero uses the
	// provider's default.
	FirstByteTimeout time.Duration
}

// Route is one provider serving a model. The route's id, as recorded in
// events and named in config, is "provider/wire id".
type Route struct {
	Provider string
	Model    string
	// WireID is the provider's id for the model when it is not the
	// canonical name.
	WireID  string
	Pricing Pricing
	// ContextWindow and MaxOutput override the model's limits when the
	// provider serves less.
	ContextWindow, MaxOutput int
}

// Models is the curated set: every model is defined once, here.
var Models = []Model{
	{Name: "claude-fable-5", Family: Anthropic, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineXML, StreamIdleTimeout: 15 * time.Minute},
	{Name: "claude-opus-5-5", Family: Anthropic, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, WorkEffort: EffortLow, CallsPerTask: 1, EditDialect: HashlineXML, StreamIdleTimeout: 15 * time.Minute},
	{Name: "claude-sonnet-5", Family: Anthropic, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineXML, StreamIdleTimeout: 10 * time.Minute},
	{Name: "claude-sonnet-5-5", Family: Anthropic, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, WorkEffort: EffortLow, CallsPerTask: 1, EditDialect: HashlineXML, StreamIdleTimeout: 10 * time.Minute},

	{Name: "gpt-5.6-sol", Family: OpenAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 15 * time.Minute},
	{Name: "gpt-5.6-terra", Family: OpenAI, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 12 * time.Minute},
	{Name: "gpt-5.6-luna", Family: OpenAI, Tier: Tiny, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},
	{Name: "gpt-6.1-sol", Family: OpenAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, CallsPerTask: 2.6, EditDialect: HashlineJSON, StreamIdleTimeout: 15 * time.Minute},
	{Name: "gpt-6-luna", Family: OpenAI, Tier: Tiny, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},

	{Name: "grok-4.5", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 500000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 15 * time.Minute},
	{Name: "grok-4.6", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 500000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 6 * time.Minute},
	{Name: "grok-4.7", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 500000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, WorkEffort: EffortLow, CallsPerTask: 2, EditDialect: HashlineJSON, StreamIdleTimeout: 10 * time.Minute},

	{Name: "kimi-k2p7-code", Family: Moonshot, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 262144, MaxOutput: 32768, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 15 * time.Minute},
	{Name: "kimi-k3", Family: Moonshot, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1048576, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},
	{Name: "qwen3p7-plus", Family: Qwen, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 32768, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, EditDialect: HashlineJSON, StreamIdleTimeout: 15 * time.Minute},
	{Name: "qwen3p8-max", Family: Qwen, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},
	{Name: "deepseek-v4-flash-0731", Family: DeepSeek, Tier: Efficient, Inputs: []Modality{Text}, ContextWindow: 1048576, MaxOutput: 128000, Effort: []Effort{EffortHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 15 * time.Minute},
	{Name: "deepseek-v4.1-flash", Family: DeepSeek, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1048576, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, WorkEffort: EffortLow, CallsPerTask: 2.7, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},
	{Name: "glm-5p3", Family: Zhipu, Tier: Efficient, Inputs: []Modality{Text}, ContextWindow: 1048576, MaxOutput: 128000, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, EditDialect: HashlineContextual, StreamIdleTimeout: 10 * time.Minute},
	{Name: "llama-3.3-70b-instruct-turbo", Family: Llama, Tier: Tiny, Inputs: []Modality{Text}, ContextWindow: 131072, MaxOutput: 16384, Effort: []Effort{EffortNone}, EditDialect: HashlineJSON, StreamIdleTimeout: 5 * time.Minute},
}

// Routes lists who serves each curated model, and at what price. Ramp prices
// are transcribed from its GET /v1/models; discovery refreshes them at
// startup.
var Routes = []Route{
	{Provider: "anthropic", Model: "claude-fable-5", Pricing: Pricing{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}},
	{Provider: "anthropic", Model: "claude-sonnet-5", Pricing: Pricing{Input: 3, Output: 15, CacheRead: .3, CacheWrite: 3.75}},
	{Provider: "openai", Model: "gpt-5.6-sol", Pricing: Pricing{Input: 5, Output: 30, CacheRead: .5, CacheWrite: 6.25, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: 10, Output: 45, CacheRead: 1, CacheWrite: 12.5}}}},
	{Provider: "openai", Model: "gpt-5.6-terra", Pricing: Pricing{Input: 2, Output: 12, CacheRead: .2, CacheWrite: 2.5, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: 4, Output: 18, CacheRead: .4, CacheWrite: 5}}}},
	{Provider: "openai", Model: "gpt-5.6-luna", Pricing: Pricing{Input: .2, Output: 1.2, CacheRead: .02, CacheWrite: .25, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: .4, Output: 1.8, CacheRead: .04, CacheWrite: .5}}}},
	{Provider: "fireworks", Model: "kimi-k2p7-code", WireID: "accounts/fireworks/models/kimi-k2p7-code", Pricing: Pricing{Input: .95, Output: 4, CacheRead: .48}},
	{Provider: "fireworks", Model: "qwen3p7-plus", WireID: "accounts/fireworks/models/qwen3p7-plus", Pricing: Pricing{Input: .60, Output: 3.60, CacheRead: .30}},
	{Provider: "xai", Model: "grok-4.5", Pricing: Pricing{Input: 2, Output: 6, CacheRead: .3, Thresholds: []ThresholdRate{{AboveTokens: 200000, Input: 4, Output: 12, CacheRead: .6}}}},
	{Provider: "xai", Model: "grok-4.6", Pricing: Pricing{Input: 2, Output: 6, CacheRead: .5}},
	{Provider: "together", Model: "deepseek-v4-flash-0731", WireID: "deepseek-ai/DeepSeek-V4-Flash-0731", Pricing: Pricing{Input: .14, Output: .28, CacheRead: .03}},
	{Provider: "together", Model: "llama-3.3-70b-instruct-turbo", WireID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Pricing: Pricing{Input: .88, Output: .88}},
	{Provider: "ramp", Model: "claude-opus-5-5", Pricing: Pricing{Input: 4, Output: 20, CacheRead: .2, CacheWrite: 5}},
	{Provider: "ramp", Model: "claude-sonnet-5-5", Pricing: Pricing{Input: 2, Output: 10, CacheRead: .2, CacheWrite: 2.5}},
	{Provider: "ramp", Model: "gpt-6.1-sol", Pricing: Pricing{Input: 2, Output: 10, CacheRead: .1}},
	{Provider: "ramp", Model: "gpt-6-luna", Pricing: Pricing{Input: .1, Output: .5, CacheRead: .01}},
	{Provider: "ramp", Model: "deepseek-v4.1-flash", Pricing: Pricing{Input: .3, Output: 1.2, CacheRead: .006}},
	{Provider: "ramp", Model: "kimi-k3", Pricing: Pricing{Input: 3, Output: 15, CacheRead: .3}},
	{Provider: "ramp", Model: "qwen3p8-max", Pricing: Pricing{Input: 2, Output: 6, CacheRead: .25}},
	{Provider: "ramp", Model: "glm-5p3", Pricing: Pricing{Input: 1.4, Output: 4.4, CacheRead: .26}},
	{Provider: "ramp", Model: "grok-4.7", Pricing: Pricing{Input: 2, Output: 6, CacheRead: .5}},
}

// ProviderCompat is how a provider's API is spoken for a model. Quirks are
// per provider, not per model: the provider's API style decides them.
func ProviderCompat(provider string, m Model) (Compat, []Effort) {
	effort := m.Effort
	reasoning := false
	for _, e := range effort {
		reasoning = reasoning || e != EffortNone
	}
	c := Compat{SupportsToolChoice: true, SupportsReasoningEffort: reasoning, StreamIdleTimeout: m.StreamIdleTimeout, FirstByteTimeout: m.FirstByteTimeout, SystemPromptStyle: SystemTopLevel}
	switch provider {
	case "anthropic":
		c.MaxTokensField, c.SupportsStrictTools, c.CacheControl = "max_tokens", true, true
	case "openai":
		c.MaxTokensField, c.RequiresAssistantText, c.SupportsStrictTools, c.CacheControl = "max_completion_tokens", true, true, true
	case "ramp":
		// Ramp Router fronts every vendor with the OpenAI Responses API. It
		// also offers "minimal"/"max" effort, which Orrery has no level for,
		// and "none", left out deliberately: Orrery drops the reasoning field
		// for EffortNone, and the gateway then applies its own default of
		// "high" while the router priced the turn as non-reasoning. Strict
		// tool schemas are on only for OpenAI models: strictifySchema makes
		// every optional parameter a nullable union, and the gateway rejects
		// other vendors' requests over its 16-union-parameter limit.
		c.MaxTokensField, c.SupportsStrictTools = "max_output_tokens", m.Family == OpenAI
		var kept []Effort
		for _, e := range effort {
			if e != EffortNone {
				kept = append(kept, e)
			}
		}
		effort = kept
	default:
		// Chat Completions gateways (xAI, Fireworks, Together).
		c.MaxTokensField, c.RequiresReasoningEcho, c.RequiresAssistantText = "max_tokens", reasoning, true
	}
	if reasoning {
		c.EffortWireMap = make(map[Effort]string, len(effort))
		for _, e := range effort {
			c.EffortWireMap[e] = string(e)
		}
	}
	return c, effort
}

// Spec builds the routable spec for a route of m.
func (r Route) Spec(m Model) ModelSpec {
	compat, effort := ProviderCompat(r.Provider, m)
	wire := r.WireID
	if wire == "" {
		wire = m.Name
	}
	s := ModelSpec{
		ID: r.Provider + "/" + wire, Model: m.Name, Family: m.Family, Tier: m.Tier,
		Inputs: append([]Modality(nil), m.Inputs...), ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput,
		Pricing: r.Pricing, Effort: effort, WorkEffort: m.WorkEffort, CallsPerTask: m.CallsPerTask, Compat: compat, EditDialect: m.EditDialect,
	}
	if r.ContextWindow > 0 {
		s.ContextWindow = r.ContextWindow
	}
	if r.MaxOutput > 0 {
		s.MaxOutput = r.MaxOutput
	}
	return s
}

// Expand returns a spec for every route, in route order. A route naming no
// curated model is a programming error.
func Expand(models []Model, routes []Route) []ModelSpec {
	byName := make(map[string]Model, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}
	out := make([]ModelSpec, 0, len(routes))
	for _, r := range routes {
		m, ok := byName[r.Model]
		if !ok {
			panic("model: route " + r.Provider + " names unknown model " + r.Model)
		}
		out = append(out, r.Spec(m))
	}
	return out
}

// Canonical finds the curated model a provider's wire id refers to: the
// canonical name itself, or a wire id one of its routes uses. Matching
// ignores case, as gateways vary it.
func Canonical(wireID string) (Model, bool) {
	for _, m := range Models {
		if strings.EqualFold(m.Name, wireID) {
			return m, true
		}
	}
	for _, r := range Routes {
		if r.WireID != "" && strings.EqualFold(r.WireID, wireID) {
			for _, m := range Models {
				if m.Name == r.Model {
					return m, true
				}
			}
		}
	}
	return Model{}, false
}
