package catalog

import (
	"slices"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
)

// Inference is deliberately conservative. A discovered model is routable only
// when the listing shows everything the harness depends on: it is active, it
// speaks the Responses API Orrery uses for Ramp, it calls tools, and it has a
// context window and prices. Discovered models are never inferred to be
// frontier tier: the frontier floor guards planning, diagnosis, and review,
// and a price list says nothing about quality. Promote a model with a config
// override once it has earned it.
const (
	// minContextWindow excludes models too small for an agent's history.
	minContextWindow = 64_000
	// efficientOutputPrice separates efficient-tier from tiny-tier models by
	// output price per million tokens: below it, a model is treated as
	// suited to cheap side tasks rather than the main loop.
	efficientOutputPrice = 0.5
	// defaultMaxOutput applies when a listing gives no output limit.
	defaultMaxOutput = 32_768
	// maxInferredOutput caps listed output limits, which can equal the
	// context window and would let one response consume it.
	maxInferredOutput = 128_000
)

// familyKeywords map model name fragments to families, checked in order.
var familyKeywords = []struct {
	fragment string
	family   model.Family
}{
	{"claude", model.Anthropic},
	{"gpt", model.OpenAI}, {"o3", model.OpenAI}, {"o4", model.OpenAI}, {"codex", model.OpenAI},
	{"grok", model.XAI},
	{"deepseek", model.DeepSeek},
	{"qwen", "qwen"},
	{"kimi", "moonshot"},
	{"llama", "llama"},
	{"glm", "zhipu"},
	{"mistral", "mistral"}, {"devstral", "mistral"}, {"codestral", "mistral"},
	{"gemini", "google"}, {"gemma", "google"},
	{"minimax", "minimax"},
}

func inferFamily(id string) model.Family {
	lower := strings.ToLower(id)
	for _, k := range familyKeywords {
		if strings.Contains(lower, k.fragment) {
			return k.family
		}
	}
	// An unknown vendor still gets a family of its own, so it diversifies
	// reviews instead of colliding with a known one.
	name := strings.FieldsFunc(lower, func(r rune) bool { return !(r >= 'a' && r <= 'z') })
	if len(name) > 0 {
		return model.Family(name[0])
	}
	return "unknown"
}

// ownKeyProviders are upstreams Ramp Router serves only with the account's
// own provider key; models available solely through them are left out unless
// the config says the account has that key.
var ownKeyProviders = []string{"bedrock"}

func inferRamp(listed []rampModel, providerKeys []string) ([]model.ModelSpec, map[string]int) {
	skipped := map[string]int{}
	var out []model.ModelSpec
	for _, m := range listed {
		if needsOwnKey(m, providerKeys) {
			skipped["needs own provider key"]++
			continue
		}
		spec, reason := inferRampModel(m)
		if reason != "" {
			skipped[reason]++
			continue
		}
		out = append(out, spec)
	}
	return out, skipped
}

// needsOwnKey reports whether every upstream serving m requires a provider key
// the account has not configured.
func needsOwnKey(m rampModel, providerKeys []string) bool {
	if len(m.Router.Providers) == 0 {
		return false
	}
	for _, p := range m.Router.Providers {
		if !slices.Contains(ownKeyProviders, p.Provider) || slices.Contains(providerKeys, p.Provider) {
			return false
		}
	}
	return true
}

// inferRampModel returns a spec, or the reason the entry is not routable.
func inferRampModel(m rampModel) (model.ModelSpec, string) {
	r := m.Router
	name := r.RequestName
	if name == "" {
		name = m.ID
	}
	switch {
	case name == "":
		return model.ModelSpec{}, "no id"
	case r.Status != "" && r.Status != "active":
		return model.ModelSpec{}, "not active"
	case !slices.Contains(r.Surfaces, "responses"):
		return model.ModelSpec{}, "no responses api"
	case !r.Capabilities.Tools.Supported:
		return model.ModelSpec{}, "no tool calling"
	case r.Limits.ContextWindow < minContextWindow:
		return model.ModelSpec{}, "context window too small"
	}
	input, okIn := price(r.Pricing.Input)
	output, okOut := price(r.Pricing.Output)
	if !okIn || !okOut {
		return model.ModelSpec{}, "no pricing"
	}
	cacheRead, okRead := price(r.Pricing.CacheRead)
	if !okRead || !r.Capabilities.PromptCaching {
		cacheRead = input
	}
	cacheWrite, okWrite := price(r.Pricing.CacheWrite)
	if !okWrite || cacheWrite == 0 {
		cacheWrite, _ = price(r.Pricing.CacheWrite5m)
	}

	family := inferFamily(name)
	tier := model.Efficient
	// Cheap models, and models without reasoning (often older generations),
	// are kept to side tasks rather than the main loop.
	if output < efficientOutputPrice || !r.Capabilities.Reasoning.Supported {
		tier = model.Tiny
	}
	inputs := []model.Modality{model.Text}
	if slices.Contains(r.Capabilities.Modalities.Input, "image") {
		inputs = append(inputs, model.Image)
	}
	maxOutput := defaultMaxOutput
	if r.Limits.MaxOutputTokens != nil && *r.Limits.MaxOutputTokens > 0 {
		maxOutput = min(*r.Limits.MaxOutputTokens, maxInferredOutput, r.Limits.ContextWindow)
	}

	// Efforts the harness understands; "minimal" and the like are dropped.
	var efforts []model.Effort
	wire := map[model.Effort]string{}
	if r.Capabilities.Reasoning.Supported {
		for _, e := range r.Capabilities.Reasoning.Efforts {
			effort := model.Effort(e.Value)
			if slices.Contains([]model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortXHigh}, effort) {
				efforts = append(efforts, effort)
				wire[effort] = e.Value
			}
		}
	}
	reasoning := len(efforts) > 0
	if !reasoning {
		efforts, wire = []model.Effort{model.EffortNone}, nil
	}

	dialect := model.HashlineContextual
	switch family {
	case model.Anthropic:
		dialect = model.HashlineXML
	case model.OpenAI:
		dialect = model.HashlineJSON
	}
	spec := model.ModelSpec{
		ID:            "ramp/" + name,
		Family:        family,
		Tier:          tier,
		Inputs:        inputs,
		ContextWindow: r.Limits.ContextWindow,
		MaxOutput:     maxOutput,
		Pricing:       model.Pricing{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite},
		Effort:        efforts,
		Compat: model.Compat{
			MaxTokensField:          "max_output_tokens",
			SupportsToolChoice:      true,
			SupportsReasoningEffort: reasoning,
			EffortWireMap:           wire,
			// Strict schemas and assistant-text requirements vary by
			// upstream; the portable settings work everywhere.
			StreamIdleTimeout: 10 * time.Minute,
			SystemPromptStyle: model.SystemTopLevel,
			CacheControl:      r.Capabilities.PromptCaching,
		},
		EditDialect: dialect,
		Discovered:  true,
	}
	return adoptCurated(spec), ""
}

// adoptCurated gives a discovered route to a curated model that model's
// definition: the listing supplies only what the route decides (price,
// limits, efforts offered). The route stays Discovered, so it does not
// outscore a curated route on price alone, but the model has its one tier
// and family everywhere. It is idempotent, so cached listings inferred
// before a model was curated pick the definition up too.
func adoptCurated(spec model.ModelSpec) model.ModelSpec {
	m, ok := model.Canonical(strings.TrimPrefix(spec.ID, "ramp/"))
	if !ok {
		return spec
	}
	m.Effort = spec.Effort
	compat, effort := model.ProviderCompat("ramp", m)
	if len(effort) == 0 {
		compat.SupportsReasoningEffort, compat.EffortWireMap, effort = false, nil, []model.Effort{model.EffortNone}
	}
	spec.Model, spec.Family, spec.Tier, spec.EditDialect = m.Name, m.Family, m.Tier, m.EditDialect
	spec.Effort, spec.Compat = effort, compat
	return spec
}
