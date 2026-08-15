package model

import "time"

type Family string

const (
	Anthropic Family = "anthropic"
	OpenAI    Family = "openai"
	XAI       Family = "xai"
	DeepSeek  Family = "deepseek"
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
	HashlineJSON EditDialect = "hashline-json"
	HashlineXML  EditDialect = "hashline-xml"
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
	{ID: "anthropic/claude-opus-4-1", Family: Anthropic, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 200000, MaxOutput: 32000, Pricing: Pricing{Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineXML},
	{ID: "anthropic/claude-sonnet-4-5", Family: Anthropic, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 200000, MaxOutput: 64000, Pricing: Pricing{Input: 3, Output: 15, CacheRead: .3, CacheWrite: 3.75, Thresholds: []ThresholdRate{{AboveTokens: 200000, Input: 6, Output: 22.5, CacheRead: .6, CacheWrite: 7.5}}}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel, CacheControl: true}, EditDialect: HashlineXML},
	{ID: "openai/gpt-5.2", Family: OpenAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 400000, MaxOutput: 128000, Pricing: Pricing{Input: 1.75, Output: 14, CacheRead: .175}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high", EffortXHigh: "xhigh"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "openai/gpt-5-mini", Family: OpenAI, Tier: Efficient, Inputs: []Modality{Text, Image}, ContextWindow: 400000, MaxOutput: 128000, Pricing: Pricing{Input: .25, Output: 2, CacheRead: .025}, Effort: []Effort{EffortLow, EffortMedium, EffortHigh}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium", EffortHigh: "high"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "openai/gpt-5-nano", Family: OpenAI, Tier: Tiny, Inputs: []Modality{Text, Image}, ContextWindow: 400000, MaxOutput: 128000, Pricing: Pricing{Input: .05, Output: .4, CacheRead: .005}, Effort: []Effort{EffortLow, EffortMedium}, Compat: Compat{MaxTokensField: "max_completion_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortMedium: "medium"}, RequiresAssistantText: true, SupportsStrictTools: true, StreamIdleTimeout: 5 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "xai/grok-4", Family: XAI, Tier: Frontier, Inputs: []Modality{Text, Image}, ContextWindow: 256000, MaxOutput: 64000, Pricing: Pricing{Input: 3, Output: 15, CacheRead: .75}, Effort: []Effort{EffortLow, EffortHigh}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, SupportsReasoningEffort: true, EffortWireMap: map[Effort]string{EffortLow: "low", EffortHigh: "high"}, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 15 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
	{ID: "together/deepseek-ai/DeepSeek-V3.1", Family: DeepSeek, Tier: Efficient, Inputs: []Modality{Text}, ContextWindow: 128000, MaxOutput: 32768, Pricing: Pricing{Input: .6, Output: 1.7, CacheRead: .15}, Effort: []Effort{EffortNone}, Compat: Compat{MaxTokensField: "max_tokens", SupportsToolChoice: true, RequiresReasoningEcho: true, RequiresAssistantText: true, StreamIdleTimeout: 10 * time.Minute, SystemPromptStyle: SystemTopLevel}, EditDialect: HashlineJSON},
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
