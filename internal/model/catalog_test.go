package model

import (
	"math"
	"testing"
)

func TestThresholdAndCachePricing(t *testing.T) {
	p := Pricing{Input: 1, Output: 2, CacheRead: .1, CacheWrite: 1.25, Thresholds: []ThresholdRate{{AboveTokens: 100, Input: 2, Output: 3, CacheRead: .2, CacheWrite: 2.5}}}
	got := p.EstimateDetailed(200, 10, 50, 50)
	want := (100*2.0 + 50*.2 + 50*2.5 + 10*3.0) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %g want %g", got, want)
	}
}
func TestCatalogIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Catalog {
		if seen[m.ID] {
			t.Fatalf("duplicate %s", m.ID)
		}
		seen[m.ID] = true
		if m.ContextWindow <= 0 || m.MaxOutput <= 0 || len(m.Effort) == 0 {
			t.Fatalf("incomplete model: %+v", m)
		}
	}
}
