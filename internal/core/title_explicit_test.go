package core

import (
	"testing"

	"github.com/ductone/orrery/internal/model"
)

// An explicit-only model is never the automatic pick for titles and memory
// extraction, however cheap it is.
func TestTitleModelSkipsExplicitOnly(t *testing.T) {
	for _, m := range model.All() {
		if m.Model == "claude-haiku-5-5" && !m.ExplicitOnly {
			t.Fatalf("%s should be explicit-only", m.ID)
		}
	}
	got := cheapestModel([]model.ModelSpec{
		{ID: "a/tiny", Tier: model.Tiny, ExplicitOnly: true, Pricing: model.Pricing{Input: .1, Output: .5}},
		{ID: "a/efficient", Tier: model.Efficient, Pricing: model.Pricing{Input: 2, Output: 10}},
	}, func(m model.ModelSpec) bool { return !m.ExplicitOnly })
	if got.ID != "a/efficient" {
		t.Errorf("picked %q", got.ID)
	}
}
