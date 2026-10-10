package core

import "github.com/ductone/orrey/internal/model"

// effectiveContextWindow is the context a model is trusted to use well:
// efficient and tiny models are capped at 250K tokens.
func effectiveContextWindow(spec model.ModelSpec) int {
	switch spec.Tier {
	case model.Efficient, model.Tiny:
		return min(spec.ContextWindow, 250_000)
	default:
		return spec.ContextWindow
	}
}
