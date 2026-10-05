package core

import (
	"testing"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
)

func TestTitleModelPrefersCuratedCheapModels(t *testing.T) {
	price := func(in, out float64) model.Pricing { return model.Pricing{Input: in, Output: out} }
	tests := []struct {
		name    string
		catalog []model.ModelSpec
		want    string
	}{
		{
			name: "curated tiny beats cheaper discovered tiny and cheaper efficient",
			catalog: []model.ModelSpec{
				{ID: "openai/found-tiny", Tier: model.Tiny, Discovered: true, Pricing: price(0.01, 0.01)},
				{ID: "openai/efficient", Tier: model.Efficient, Pricing: price(0.1, 0.1)},
				{ID: "openai/tiny-pricey", Tier: model.Tiny, Pricing: price(1, 2)},
				{ID: "openai/tiny", Tier: model.Tiny, Pricing: price(0.5, 1)},
			},
			want: "openai/tiny",
		},
		{
			name: "curated efficient when no curated tiny",
			catalog: []model.ModelSpec{
				{ID: "openai/found-tiny", Tier: model.Tiny, Discovered: true, Pricing: price(0.01, 0.01)},
				{ID: "openai/frontier", Tier: model.Frontier, Pricing: price(0.2, 0.2)},
				{ID: "openai/efficient-pricey", Tier: model.Efficient, Pricing: price(3, 3)},
				{ID: "openai/efficient", Tier: model.Efficient, Pricing: price(2, 2)},
			},
			want: "openai/efficient",
		},
		{
			name: "any curated model when no curated tiny or efficient",
			catalog: []model.ModelSpec{
				{ID: "openai/found-tiny", Tier: model.Tiny, Discovered: true, Pricing: price(0.01, 0.01)},
				{ID: "openai/frontier-pricey", Tier: model.Frontier, Pricing: price(20, 20)},
				{ID: "openai/frontier", Tier: model.Frontier, Pricing: price(10, 10)},
			},
			want: "openai/frontier",
		},
		{
			name: "cheapest discovered when nothing curated",
			catalog: []model.ModelSpec{
				{ID: "openai/found-b", Tier: model.Tiny, Discovered: true, Pricing: price(2, 2)},
				{ID: "openai/found-a", Tier: model.Tiny, Discovered: true, Pricing: price(1, 1)},
			},
			want: "openai/found-a",
		},
		{
			name: "unavailable curated models are skipped",
			catalog: []model.ModelSpec{
				{ID: "other/tiny", Tier: model.Tiny, Pricing: price(0.01, 0.01)},
				{ID: "openai/efficient", Tier: model.Efficient, Pricing: price(2, 2)},
			},
			want: "openai/efficient",
		},
		{
			name:    "no available model",
			catalog: []model.ModelSpec{{ID: "other/tiny", Tier: model.Tiny}},
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model.Install(tc.catalog)
			t.Cleanup(func() { model.Install(model.Catalog) })
			registry := provider.New(config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "test"}}})
			if got := titleModel(registry).ID; got != tc.want {
				t.Fatalf("titleModel = %q, want %q", got, tc.want)
			}
		})
	}
}
