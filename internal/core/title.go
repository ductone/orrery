package core

import (
	"context"
	"time"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
	"github.com/ductone/orrey/internal/router"
	"github.com/ductone/orrey/internal/store"
)

const (
	// maxTitleChars is the hard cap for the displayed/generated title.
	// titleMaxOutput limits how many tokens the lightweight model can emit.
	titleMaxOutput = 100
	// titleTimeout caps the best-effort title generation so it never blocks the session.
	titleTimeout = 30 * time.Second
)

var titlePrompt = store.TitlePrompt

// generateSessionTitle attempts to replace the deterministic fallback title with a concise,
// model-generated summary. It runs in a detached goroutine and swallows all errors so session
// execution is never affected.
func (e *Engine) generateSessionTitle(ctx context.Context, sid, spec string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), titleTimeout)
		defer cancel()

		_, registry, _, _, _ := e.runtimeSnapshot()

		m := titleModel(registry)
		if m.ID == "" {
			return
		}

		decision := router.Decision{
			Model:  m,
			Effort: model.EffortLow,
		}

		build := func(m model.ModelSpec, d router.Decision) (provider.Request, error) {
			return provider.Request{
				System:    titlePrompt,
				Messages:  []provider.Message{{Role: "user", Content: "User request:\n" + spec}},
				MaxOutput: min(titleMaxOutput, m.MaxOutput),
				Effort:    d.Effort,
			}, nil
		}

		resp, err := registry.CompleteOne(ctx, decision, build)
		if err != nil {
			return
		}
		title := store.NormalizeTitle(resp.Message.Content)
		if title == "" {
			return
		}
		_ = e.store.UpdateSessionTitle(ctx, sid, title)
	}()
}

// titleModel picks the model for best-effort title generation: the cheapest
// available curated Tiny model, then the cheapest curated Efficient model,
// then the cheapest curated model of any tier. Discovered models are a last
// resort, since an untested discovered model can return unparseable output.
func titleModel(registry *provider.Registry) model.ModelSpec {
	var candidates []model.ModelSpec
	for _, m := range model.All() {
		if registry.Available(m) {
			candidates = append(candidates, m)
		}
	}
	for _, accept := range []func(model.ModelSpec) bool{
		func(m model.ModelSpec) bool { return !m.Discovered && m.Tier == model.Tiny },
		func(m model.ModelSpec) bool { return !m.Discovered && m.Tier == model.Efficient },
		func(m model.ModelSpec) bool { return !m.Discovered },
		func(model.ModelSpec) bool { return true },
	} {
		if m := cheapestModel(candidates, accept); m.ID != "" {
			return m
		}
	}
	return model.ModelSpec{}
}

// cheapestModel returns the accepted model with the lowest combined
// input/output cost per 1M tokens, or a zero spec when none is accepted.
func cheapestModel(models []model.ModelSpec, accept func(model.ModelSpec) bool) model.ModelSpec {
	var best model.ModelSpec
	var bestCost float64 = -1
	for _, m := range models {
		if !accept(m) {
			continue
		}
		cost := m.Pricing.Input + m.Pricing.Output
		if bestCost < 0 || cost < bestCost {
			bestCost = cost
			best = m
		}
	}
	return best
}
