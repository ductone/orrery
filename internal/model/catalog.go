package model

import (
	"sync/atomic"
	"time"
)

type Family string

const (
	Anthropic Family = "anthropic"
	OpenAI    Family = "openai"
	XAI       Family = "xai"
	DeepSeek  Family = "deepseek"
	Moonshot  Family = "moonshot"
	Qwen      Family = "qwen"
	Zhipu     Family = "zhipu"
	Llama     Family = "llama"
	// Fireworks named the family of Fireworks-served models before families
	// meant labs; config written then may still use it.
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
	FirstByteTimeout                                                  time.Duration
	SystemPromptStyle                                                 SystemStyle
	CacheControl                                                      bool
}

// ModelSpec is a routable model: one route to a model, with everything the
// router and the provider clients need.
type ModelSpec struct {
	ID string
	// Model is the canonical model this route serves, when it is curated.
	Model                    string `json:",omitempty"`
	Family                   Family
	Tier                     Tier
	Inputs                   []Modality
	ContextWindow, MaxOutput int
	Pricing                  Pricing
	Effort                   []Effort
	WorkEffort               Effort `json:",omitempty"`
	Compat                   Compat
	EditDialect              EditDialect
	// Discovered marks a model inferred from a provider listing rather than
	// built in or vouched for by a config override that sets its tier.
	Discovered bool `json:",omitempty"`
}

// Catalog is the built-in routes: one spec per Route, carrying its Model's
// judgement and its provider's API settings.
var Catalog = Expand(Models, Routes)

// active is the catalog in use: the built-in Catalog until Install replaces
// it with one merged from discovery and configuration. It is swapped whole, so
// readers never see a partial catalog during a runtime reload.
var active atomic.Pointer[[]ModelSpec]

// Install replaces the active catalog. Runtime objects built afterwards (the
// router, provider registry lookups) see the new models.
func Install(specs []ModelSpec) {
	c := append([]ModelSpec(nil), specs...)
	active.Store(&c)
}

// All returns the active catalog. Callers must not modify it.
func All() []ModelSpec {
	if c := active.Load(); c != nil {
		return *c
	}
	return Catalog
}

func Get(id string) (ModelSpec, bool) {
	for _, m := range All() {
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
