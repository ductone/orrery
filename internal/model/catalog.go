package model

import "time"

type Family string

const (
	Anthropic Family = "anthropic"
	OpenAI    Family = "openai"
	XAI       Family = "xai"
	DeepSeek  Family = "deepseek"
	Fireworks Family = "fireworks"
)

type Tier string

const (
	Frontier  Tier = "frontier"
	Efficient Tier = "efficient"
	Tiny      Tier = "tiny"
)

type Modality string

const (
	Text  Modality = "text"
	Image Modality = "image"
)

type Effort string

const (
	EffortNone   Effort = "none"
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
)

type SystemStyle string

const (
	SystemTopLevel  SystemStyle = "top-level"
	SystemFirstTurn SystemStyle = "first-turn"
)

type EditDialect string

const (
	HashlineJSON       EditDialect = "hashline-json"
	HashlineXML        EditDialect = "hashline-xml"
	HashlineContextual EditDialect = "hashline-contextual"
	TextAnchor         EditDialect = "text-anchor"
)

type ThresholdRate struct {
	AboveTokens                          int
	Input, Output, CacheRead, CacheWrite float64
}
type Pricing struct {
	Input, Output, CacheRead, CacheWrite float64
	Thresholds                           []ThresholdRate
}

func (p Pricing) Rates(tokens int) Pricing {
	r := p
	for _, t := range p.Thresholds {
		if tokens > t.AboveTokens {
			r.Input, r.Output, r.CacheRead, r.CacheWrite = t.Input, t.Output, t.CacheRead, t.CacheWrite
		}
	}
	return r
}
func (p Pricing) Estimate(input, output, cached int) float64 {
	r := p.Rates(input)
	fresh := input - cached
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*r.Input + float64(cached)*r.CacheRead + float64(output)*r.Output) / 1e6
}
func (p Pricing) EstimateDetailed(input, output, cacheRead, cacheWrite int) float64 {
	r := p.Rates(input)
	fresh := input - cacheRead - cacheWrite
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*r.Input + float64(cacheRead)*r.CacheRead + float64(cacheWrite)*r.CacheWrite + float64(output)*r.Output) / 1e6
}

type Compat struct {
	MaxTokensField                                                    string
	SupportsToolChoice, SupportsReasoningEffort                       bool
	EffortWireMap                                                     map[Effort]string
	RequiresReasoningEcho, RequiresAssistantText, SupportsStrictTools bool
	StreamIdleTimeout                                                 time.Duration
	SystemPromptStyle                                                 SystemStyle
	CacheControl                                                      bool
}
type ModelSpec struct {
	ID                       string
	Family                   Family
	Tier                     Tier
	Inputs                   []Modality
	ContextWindow, MaxOutput int
	Pricing                  Pricing
	Effort                   []Effort
	Compat                   Compat
	EditDialect              EditDialect
}

var Catalog = []ModelSpec{
	{ID: "anthropic/claude-fable-5", Family: Anthropic, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Pricing: Pricing{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high", EffortXHigh: "xhigh"}, SupportsStrictTools: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineXML},
	{ID: "anthropic/claude-sonnet-5", Family: Anthropic, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 128000, Pricing: Pricing{Input: 3, Output: 15, CacheRead: .3, CacheWrite: 3.75}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high", EffortXHigh: "xhigh"}, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineXML},
	{ID: "openai/gpt-5.6-sol", Family: OpenAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Pricing: Pricing{Input: 5, Output: 30, CacheRead: .5, CacheWrite: 6.25, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: 10, Output: 45, CacheRead: 1, CacheWrite: 12.5}}}, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortNone: "none", EffortLow: "low", EffortMedium: "medium", EffortHigh: "high", EffortXHigh: "xhigh"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineJSON},
	{ID: "openai/gpt-5.6-terra", Family: OpenAI, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Pricing: Pricing{Input: 2, Output: 12, CacheRead: .2, CacheWrite: 2.5, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: 4, Output: 18, CacheRead: .4, CacheWrite: 5}}}, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortNone: "none", EffortLow: "low", EffortMedium: "medium", EffortHigh: "high", EffortXHigh: "xhigh"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 12 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineJSON},
	{ID: "openai/gpt-5.6-luna", Family: OpenAI, Tier: Tiny, Inputs: []Modality{Text, Image}, ContextWindow: 1050000, MaxOutput: 128000, Pricing: Pricing{Input: .2, Output: 1.2, CacheRead: .02, CacheWrite: .25, Thresholds: []ThresholdRate{{AboveTokens: 272000, Input: .4, Output: 1.8, CacheRead: .04, CacheWrite: .5}}}, Effort: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortNone: "none", EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineContextual},
	{ID: "fireworks/accounts/fireworks/models/kimi-k2p7-code", Family: Fireworks, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 262144, MaxOutput: 32768, Pricing: Pricing{Input: .95, Output: 4, CacheRead: .48}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "fireworks/accounts/fireworks/models/qwen3p7-plus", Family: Fireworks, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 1000000, MaxOutput: 32768, Pricing: Pricing{Input: .60, Output: 3.60, CacheRead: .30}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "xai/grok-4.5", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 500000, MaxOutput: 128000, Pricing: Pricing{Input: 2, Output: 6, CacheRead: .3, Thresholds: []ThresholdRate{{AboveTokens: 200000, Input: 4, Output: 12, CacheRead: .6}}}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "xai/grok-4.6", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 500000, MaxOutput: 128000, Pricing: Pricing{Input: 2, Output: 6, CacheRead: .5}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 6 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "together/deepseek-ai/DeepSeek-V4-Flash-0731", Family: DeepSeek, Tier: Efficient, Inputs: []Modality{Text}, ContextWindow: 1048576, MaxOutput: 128000, Pricing: Pricing{Input: .14, Output: .28, CacheRead: .03}, Effort: []Effort{EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: TextAnchor},
	{ID: "together/meta-llama/Llama-3.3-70B-Instruct-Turbo", Family: "llama", Tier: Tiny, Inputs: []Modality{Text}, ContextWindow: 131072, MaxOutput: 16384, Pricing: Pricing{Input: .88, Output: .88}, Effort: []Effort{EffortNone}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, RequiresAssistantText: true, StreamIdleTimeout: 5 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
}

func Get(id string) (ModelSpec, bool) {
	for _, m := range Catalog {
		if m.ID == id {
			return m, true
		}
	}
	return ModelSpec{}, false
}
func Supports(m ModelSpec, x Modality) bool {
	for _, in := range m.Inputs {
		if in == x {
			return true
		}
	}
	return false
}
