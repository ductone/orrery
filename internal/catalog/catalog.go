package catalog

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
)

// Result is a built catalog and how it was assembled.
type Result struct {
	Models      []model.ModelSpec
	Discoveries []Discovery
	// Discovered counts models taken from discovery (not shadowed by a
	// built-in entry); Overridden counts config entries applied.
	Discovered, Overridden, Disabled int
	// Refused counts models left out because a provider recently refused
	// them for this account.
	Refused int
	// Warnings are config entries that could not be applied.
	Warnings []string
}

// Build assembles the catalog for cfg: providers that list their models are
// asked (Ramp today), the built-in catalog wins over what they report for the
// same id, and cfg.Models overrides both field by field.
func Build(ctx context.Context, cfg config.Config, cacheDir string, client *http.Client) Result {
	if client == nil {
		client = &http.Client{Timeout: discoveryTimeout}
	}
	var res Result
	var discovered []model.ModelSpec
	if p, ok := cfg.Providers["ramp"]; ok {
		if key := firstKey(p); key != "" {
			base := p.BaseURL
			if base == "" {
				base = DefaultRampBaseURL
			}
			d := DiscoverRamp(ctx, client, base, key, cacheDir, p.ProviderKeys)
			res.Discoveries = append(res.Discoveries, d)
			discovered = append(discovered, d.Models...)
		}
	}
	// Models a provider refused for this account recently are left out of
	// discovery, so a restart does not route to them again.
	refused := Unavailable(cacheDir, time.Now())
	discovered = slices.DeleteFunc(discovered, func(m model.ModelSpec) bool { _, ok := refused[m.ID]; return ok })
	res.Refused = len(refused)
	res.Models, res.Discovered = Merge(discovered, model.Catalog)
	res.Models, res.Overridden, res.Disabled, res.Warnings = Apply(res.Models, cfg.Models)
	return res
}

func firstKey(p config.ProviderConfig) string {
	if p.APIKey != "" {
		return p.APIKey
	}
	for _, k := range p.Keys {
		if k != "" {
			return k
		}
	}
	return ""
}

// Merge layers built-in specs over discovered ones: a built-in entry replaces
// a discovered model with the same id, because its compatibility settings and
// tier were chosen deliberately. Built-in order comes first, then new
// discovered models in listing order.
func Merge(discovered, builtin []model.ModelSpec) ([]model.ModelSpec, int) {
	known := map[string]bool{}
	out := make([]model.ModelSpec, 0, len(builtin)+len(discovered))
	for _, m := range builtin {
		known[m.ID] = true
		out = append(out, m)
	}
	added := 0
	for _, m := range discovered {
		if known[m.ID] {
			continue
		}
		known[m.ID] = true
		out = append(out, m)
		added++
	}
	return out, added
}

// Apply merges config overrides into a catalog. An override for a known id
// replaces only the fields it sets; one for an unknown id adds a model when
// it is complete, and is otherwise reported as a warning (the model may simply
// not have been discovered this time). disabled removes a model.
func Apply(models []model.ModelSpec, overrides []config.ModelConfig) ([]model.ModelSpec, int, int, []string) {
	out := slices.Clone(models)
	index := map[string]int{}
	for i, m := range out {
		index[m.ID] = i
	}
	var warnings []string
	applied, disabled := 0, 0
	remove := map[string]bool{}
	for _, o := range overrides {
		if o.Disabled != nil && *o.Disabled {
			if _, ok := index[o.ID]; ok {
				remove[o.ID] = true
				disabled++
			}
			continue
		}
		i, ok := index[o.ID]
		if !ok {
			spec, err := newFromOverride(o)
			if err != nil {
				warnings = append(warnings, err.Error())
				continue
			}
			index[o.ID] = len(out)
			out = append(out, spec)
			applied++
			continue
		}
		out[i] = override(out[i], o)
		applied++
	}
	if len(remove) > 0 {
		out = slices.DeleteFunc(out, func(m model.ModelSpec) bool { return remove[m.ID] })
	}
	return out, applied, disabled, warnings
}

func newFromOverride(o config.ModelConfig) (model.ModelSpec, error) {
	if o.Family == nil || o.Tier == nil || o.ContextWindow == nil || o.MaxOutput == nil || o.Pricing == nil || o.Pricing.Input == nil || o.Pricing.Output == nil {
		return model.ModelSpec{}, fmt.Errorf("models: %s is not in the catalog, and an entry that adds a model needs family, tier, context_window, max_output, and pricing input and output", o.ID)
	}
	base := model.ModelSpec{
		ID:     o.ID,
		Inputs: []model.Modality{model.Text},
		Effort: []model.Effort{model.EffortNone},
		Compat: model.Compat{MaxTokensField: "max_output_tokens", SupportsToolChoice: true, SystemPromptStyle: model.SystemTopLevel},
		// The contextual dialect tolerates duplicate lines best.
		EditDialect: model.HashlineContextual,
	}
	return override(base, o), nil
}

// override returns m with every field o sets.
func override(m model.ModelSpec, o config.ModelConfig) model.ModelSpec {
	if o.Family != nil {
		m.Family = *o.Family
	}
	if o.Tier != nil {
		m.Tier = *o.Tier
	}
	if o.Inputs != nil {
		m.Inputs = slices.Clone(*o.Inputs)
	}
	if o.ContextWindow != nil {
		m.ContextWindow = *o.ContextWindow
	}
	if o.MaxOutput != nil {
		m.MaxOutput = *o.MaxOutput
	}
	if o.Effort != nil {
		m.Effort = slices.Clone(*o.Effort)
	}
	if o.EditDialect != nil {
		m.EditDialect = *o.EditDialect
	}
	if p := o.Pricing; p != nil {
		if p.Input != nil {
			m.Pricing.Input = *p.Input
		}
		if p.Output != nil {
			m.Pricing.Output = *p.Output
		}
		if p.CacheRead != nil {
			m.Pricing.CacheRead = *p.CacheRead
		}
		if p.CacheWrite != nil {
			m.Pricing.CacheWrite = *p.CacheWrite
		}
		if p.Thresholds != nil {
			m.Pricing.Thresholds = nil
			for _, t := range *p.Thresholds {
				m.Pricing.Thresholds = append(m.Pricing.Thresholds, model.ThresholdRate{AboveTokens: t.AboveTokens, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite})
			}
		}
	}
	if c := o.Compat; c != nil {
		if c.MaxTokensField != nil {
			m.Compat.MaxTokensField = *c.MaxTokensField
		}
		if c.SupportsToolChoice != nil {
			m.Compat.SupportsToolChoice = *c.SupportsToolChoice
		}
		if c.SupportsReasoningEffort != nil {
			m.Compat.SupportsReasoningEffort = *c.SupportsReasoningEffort
		}
		if c.EffortWireMap != nil {
			m.Compat.EffortWireMap = maps.Clone(*c.EffortWireMap)
		}
		if c.RequiresReasoningEcho != nil {
			m.Compat.RequiresReasoningEcho = *c.RequiresReasoningEcho
		}
		if c.RequiresAssistantText != nil {
			m.Compat.RequiresAssistantText = *c.RequiresAssistantText
		}
		if c.SupportsStrictTools != nil {
			m.Compat.SupportsStrictTools = *c.SupportsStrictTools
		}
		if c.StreamIdleTimeout != nil {
			m.Compat.StreamIdleTimeout = *c.StreamIdleTimeout
		}
		if c.SystemPromptStyle != nil {
			m.Compat.SystemPromptStyle = *c.SystemPromptStyle
		}
		if c.CacheControl != nil {
			m.Compat.CacheControl = *c.CacheControl
		}
	}
	return m
}
