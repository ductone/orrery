package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
)

// entry builds a Ramp /v1/models entry in the listing's shape.
func entry(name string, mutate func(map[string]any)) map[string]any {
	e := map[string]any{
		"id": name, "object": "model", "owned_by": "router",
		"router": map[string]any{
			"request_name": name,
			"status":       "active",
			"surfaces":     []any{"responses", "messages"},
			"limits":       map[string]any{"context_window": 1048576, "max_input_tokens": nil, "max_output_tokens": 1048576},
			"capabilities": map[string]any{
				"modalities":     map[string]any{"input": []any{"image", "text"}, "output": []any{"text"}},
				"tools":          map[string]any{"supported": true},
				"prompt_caching": true,
				"reasoning": map[string]any{"supported": true, "efforts": []any{
					map[string]any{"value": "none"}, map[string]any{"value": "minimal"}, map[string]any{"value": "low"},
					map[string]any{"value": "medium"}, map[string]any{"value": "high"}, map[string]any{"value": "xhigh"},
				}},
			},
			"pricing": map[string]any{"input": "0.3", "output": "1.2", "cache_read_input": "0.006", "cache_write_input": "0", "cache_write_input_5m": "0"},
		},
	}
	if mutate != nil {
		mutate(e["router"].(map[string]any))
	}
	return e
}

func decode(t *testing.T, entries ...map[string]any) []rampModel {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"data": entries})
	var out struct {
		Data []rampModel `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Data
}

func TestInferOpenModel(t *testing.T) {
	specs, skipped := inferRamp(decode(t, entry("deepseek-v4.1-flash", nil)), nil)
	if len(specs) != 1 || len(skipped) != 0 {
		t.Fatalf("specs=%v skipped=%v", specs, skipped)
	}
	m := specs[0]
	if m.ID != "ramp/deepseek-v4.1-flash" || m.Family != model.DeepSeek || m.Tier != model.Efficient {
		t.Fatalf("identity = %s %s %s", m.ID, m.Family, m.Tier)
	}
	if !model.Supports(m, model.Image) || m.ContextWindow != 1048576 || m.MaxOutput != maxInferredOutput {
		t.Fatalf("limits = %+v", m)
	}
	if m.Pricing.Input != 0.3 || m.Pricing.Output != 1.2 || m.Pricing.CacheRead != 0.006 {
		t.Fatalf("pricing = %+v", m.Pricing)
	}
	if !slices.Equal(m.Effort, []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortXHigh}) || m.Compat.EffortWireMap[model.EffortHigh] != "high" || !m.Compat.SupportsReasoningEffort {
		t.Fatalf("effort = %v %v", m.Effort, m.Compat.EffortWireMap)
	}
	if m.EditDialect != model.HashlineContextual || m.Compat.SupportsStrictTools || !m.Compat.CacheControl || m.Compat.MaxTokensField != "max_output_tokens" {
		t.Fatalf("compat = %+v dialect %s", m.Compat, m.EditDialect)
	}
}

func TestInferenceRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		skip   string
		check  func(*testing.T, model.ModelSpec)
	}{
		{"inactive", func(r map[string]any) { r["status"] = "deprecated" }, "not active", nil},
		{"messages only", func(r map[string]any) { r["surfaces"] = []any{"messages"} }, "no responses api", nil},
		{"no tools", func(r map[string]any) {
			r["capabilities"].(map[string]any)["tools"] = map[string]any{"supported": false}
		}, "no tool calling", nil},
		{"small context", func(r map[string]any) { r["limits"] = map[string]any{"context_window": 32000} }, "context window too small", nil},
		{"no price", func(r map[string]any) { r["pricing"] = map[string]any{"input": ""} }, "no pricing", nil},
		{"cheap is tiny", func(r map[string]any) {
			r["pricing"] = map[string]any{"input": "0.02", "output": "0.1"}
		}, "", func(t *testing.T, m model.ModelSpec) {
			if m.Tier != model.Tiny || m.Pricing.CacheRead != 0.02 {
				t.Fatalf("tier %s cache %v", m.Tier, m.Pricing.CacheRead)
			}
		}},
		{"expensive is still never frontier", func(r map[string]any) {
			r["pricing"] = map[string]any{"input": "15", "output": "75"}
		}, "", func(t *testing.T, m model.ModelSpec) {
			if m.Tier != model.Efficient {
				t.Fatalf("tier = %s", m.Tier)
			}
		}},
		{"no reasoning", func(r map[string]any) {
			r["capabilities"].(map[string]any)["reasoning"] = map[string]any{"supported": false}
		}, "", func(t *testing.T, m model.ModelSpec) {
			if m.Compat.SupportsReasoningEffort || !slices.Equal(m.Effort, []model.Effort{model.EffortNone}) {
				t.Fatalf("effort %v reasoning %v", m.Effort, m.Compat.SupportsReasoningEffort)
			}
		}},
		{"text only, no output limit", func(r map[string]any) {
			r["capabilities"].(map[string]any)["modalities"] = map[string]any{"input": []any{"text"}}
			r["limits"] = map[string]any{"context_window": 200000}
		}, "", func(t *testing.T, m model.ModelSpec) {
			if model.Supports(m, model.Image) || m.MaxOutput != defaultMaxOutput {
				t.Fatalf("inputs %v max output %d", m.Inputs, m.MaxOutput)
			}
		}},
		{"no prompt caching prices cached reads as fresh", func(r map[string]any) {
			r["capabilities"].(map[string]any)["prompt_caching"] = false
		}, "", func(t *testing.T, m model.ModelSpec) {
			if m.Pricing.CacheRead != m.Pricing.Input || m.Compat.CacheControl {
				t.Fatalf("pricing %+v", m.Pricing)
			}
		}},
	} {
		specs, skipped := inferRamp(decode(t, entry("qwen4-coder", tc.mutate)), nil)
		if tc.skip != "" {
			if len(specs) != 0 || skipped[tc.skip] != 1 {
				t.Errorf("%s: specs=%v skipped=%v", tc.name, specs, skipped)
			}
			continue
		}
		if len(specs) != 1 {
			t.Errorf("%s: skipped %v", tc.name, skipped)
			continue
		}
		t.Run(tc.name, func(t *testing.T) { tc.check(t, specs[0]) })
	}
}

func TestInferFamily(t *testing.T) {
	for id, want := range map[string]model.Family{
		"claude-opus-5": model.Anthropic, "gpt-6.1-sol": model.OpenAI, "grok-5": model.XAI,
		"deepseek-v4.1-flash": model.DeepSeek, "qwen4-coder-480b": "qwen", "kimi-k3": "moonshot",
		"glm-5": "zhipu", "devstral-2": "mistral", "gemini-3-pro": "google", "acme-7b": "acme",
	} {
		if got := inferFamily(id); got != want {
			t.Errorf("inferFamily(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestMergePrefersBuiltInEntries(t *testing.T) {
	builtin := []model.ModelSpec{{ID: "ramp/claude-opus-5", Tier: model.Frontier}}
	discovered := []model.ModelSpec{{ID: "ramp/claude-opus-5", Tier: model.Efficient}, {ID: "ramp/qwen4-coder", Tier: model.Efficient}}
	merged, added := Merge(discovered, builtin)
	if len(merged) != 2 || added != 1 || merged[0].Tier != model.Frontier || merged[1].ID != "ramp/qwen4-coder" {
		t.Fatalf("merged=%v added=%d", merged, added)
	}
}

func ptr[T any](v T) *T { return &v }

func TestApplyOverrides(t *testing.T) {
	models := []model.ModelSpec{
		{ID: "ramp/qwen4-coder", Family: "qwen", Tier: model.Efficient, ContextWindow: 1000, MaxOutput: 100, Pricing: model.Pricing{Input: 1, Output: 2, CacheRead: .1}, Compat: model.Compat{CacheControl: true}},
		{ID: "ramp/noisy", Tier: model.Tiny},
	}
	overrides := []config.ModelConfig{
		{ID: "ramp/qwen4-coder", Tier: ptr(model.Frontier), Pricing: &config.ModelPricingConfig{Output: ptr(1.5)}, Compat: &config.ModelCompatConfig{SupportsStrictTools: ptr(true)}},
		{ID: "ramp/noisy", Disabled: ptr(true)},
		{ID: "ramp/brand-new", Family: ptr(model.Family("acme")), Tier: ptr(model.Efficient), ContextWindow: ptr(200000), MaxOutput: ptr(8000), Pricing: &config.ModelPricingConfig{Input: ptr(0.1), Output: ptr(0.4)}},
		{ID: "ramp/not-listed-today", Tier: ptr(model.Frontier)},
	}
	out, applied, disabled, warnings := Apply(models, overrides)
	if applied != 2 || disabled != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], "not-listed-today") {
		t.Fatalf("applied=%d disabled=%d warnings=%v", applied, disabled, warnings)
	}
	byID := map[string]model.ModelSpec{}
	for _, m := range out {
		byID[m.ID] = m
	}
	q := byID["ramp/qwen4-coder"]
	if q.Tier != model.Frontier || q.Pricing.Output != 1.5 || q.Pricing.Input != 1 || q.Pricing.CacheRead != .1 || !q.Compat.SupportsStrictTools || !q.Compat.CacheControl || q.ContextWindow != 1000 {
		t.Fatalf("field-level override wrong: %+v", q)
	}
	if _, ok := byID["ramp/noisy"]; ok {
		t.Fatal("a disabled model must be removed")
	}
	n, ok := byID["ramp/brand-new"]
	if !ok || n.Family != "acme" || n.EditDialect != model.HashlineContextual || n.Pricing.Output != 0.4 || len(n.Effort) == 0 {
		t.Fatalf("added model = %+v", n)
	}
	// The input is not modified.
	if models[0].Tier != model.Efficient {
		t.Fatal("Apply must not modify its input")
	}
}

func rampServer(t *testing.T, status int, entries ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": entries})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBuildDiscoversCachesAndFallsBack(t *testing.T) {
	cache := t.TempDir()
	live := rampServer(t, 200, entry("deepseek-v4.2-flash", nil), entry("claude-opus-5", nil), entry("old-model", func(r map[string]any) { r["status"] = "retired" }))
	cfg := config.Config{
		Providers: map[string]config.ProviderConfig{"ramp": {APIKey: "k", BaseURL: live.URL}},
		Models:    []config.ModelConfig{{ID: "ramp/deepseek-v4.2-flash", Tier: ptr(model.Tiny)}},
	}
	res := Build(context.Background(), cfg, cache, nil)
	if len(res.Discoveries) != 1 || res.Discoveries[0].Source != "live" || res.Discoveries[0].Listed != 3 || res.Discoveries[0].Skipped["not active"] != 1 {
		t.Fatalf("discovery = %+v", res.Discoveries)
	}
	if res.Discovered != 1 || res.Overridden != 1 {
		t.Fatalf("discovered=%d overridden=%d", res.Discovered, res.Overridden)
	}
	var flash, opus model.ModelSpec
	for _, m := range res.Models {
		switch m.ID {
		case "ramp/deepseek-v4.2-flash":
			flash = m
		case "ramp/claude-opus-5":
			opus = m
		}
	}
	if flash.Tier != model.Tiny {
		t.Fatalf("the override must win over inference: %+v", flash)
	}
	if opus.Tier != model.Frontier {
		t.Fatalf("the built-in entry must win over discovery: %+v", opus)
	}

	// The provider is down: the cached listing is used.
	cfg.Providers["ramp"] = config.ProviderConfig{APIKey: "k", BaseURL: rampServer(t, 503).URL}
	res = Build(context.Background(), cfg, cache, nil)
	if d := res.Discoveries[0]; d.Source != "cache" || !strings.Contains(d.Error, "503") || res.Discovered != 1 {
		t.Fatalf("fallback = %+v discovered %d", d, res.Discovered)
	}
	// No cache either: the built-in catalog alone.
	res = Build(context.Background(), cfg, t.TempDir(), nil)
	if d := res.Discoveries[0]; d.Source != "none" || res.Discovered != 0 || len(res.Models) != len(model.Catalog) {
		t.Fatalf("no-cache fallback = %+v models %d", d, len(res.Models))
	}
}

func TestBuildWithoutRampSkipsDiscovery(t *testing.T) {
	res := Build(context.Background(), config.Config{Providers: map[string]config.ProviderConfig{"openai": {APIKey: "k"}}}, t.TempDir(), nil)
	if len(res.Discoveries) != 0 || len(res.Models) != len(model.Catalog) {
		t.Fatalf("res = %+v", res)
	}
}

func TestOwnKeyOnlyModelsAreLeftOutUnlessConfigured(t *testing.T) {
	bedrockOnly := entry("us.openai.gpt-6-luna", func(r map[string]any) {
		r["providers"] = []any{map[string]any{"provider": "bedrock"}}
	})
	mixed := entry("gpt-6-luna", func(r map[string]any) {
		r["providers"] = []any{map[string]any{"provider": "bedrock"}, map[string]any{"provider": "openai"}}
	})
	specs, skipped := inferRamp(decode(t, bedrockOnly, mixed), nil)
	if len(specs) != 1 || specs[0].ID != "ramp/gpt-6-luna" || skipped["needs own provider key"] != 1 {
		t.Fatalf("specs=%v skipped=%v", specs, skipped)
	}
	specs, _ = inferRamp(decode(t, bedrockOnly), []string{"bedrock"})
	if len(specs) != 1 {
		t.Fatal("provider_keys: [bedrock] must bring Bedrock-only models back")
	}
}

func TestRefusedModelsAreLeftOutUntilTheyExpire(t *testing.T) {
	dir := t.TempDir()
	if err := MarkUnavailable(dir, "ramp/deepseek-v4.2-flash", "provider_key_required"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got := Unavailable(dir, now); got["ramp/deepseek-v4.2-flash"] != "provider_key_required" {
		t.Fatalf("unavailable = %v", got)
	}
	if got := Unavailable(dir, now.Add(refusalTTL+time.Minute)); len(got) != 0 {
		t.Fatalf("refusals must expire: %v", got)
	}
	srv := rampServer(t, 200, entry("deepseek-v4.2-flash", nil), entry("qwen4-coder", nil))
	res := Build(context.Background(), config.Config{Providers: map[string]config.ProviderConfig{"ramp": {APIKey: "k", BaseURL: srv.URL}}}, dir, nil)
	ids := map[string]bool{}
	for _, m := range res.Models {
		ids[m.ID] = true
	}
	if ids["ramp/deepseek-v4.2-flash"] || !ids["ramp/qwen4-coder"] || res.Refused != 1 {
		t.Fatalf("refused model must be left out: refused=%d ids=%v", res.Refused, ids)
	}
}

func TestDiscoveredFlagAndReasoningTier(t *testing.T) {
	specs, _ := inferRamp(decode(t, entry("gpt-4o-mini", func(r map[string]any) {
		r["capabilities"].(map[string]any)["reasoning"] = map[string]any{"supported": false}
	}), entry("qwen4-coder", nil)), nil)
	if len(specs) != 2 || !specs[0].Discovered || specs[0].Tier != model.Tiny || specs[1].Tier != model.Efficient {
		t.Fatalf("specs = %+v", specs)
	}
	out, _, _, _ := Apply(specs, []config.ModelConfig{{ID: "ramp/qwen4-coder", Tier: ptr(model.Frontier)}, {ID: "ramp/gpt-4o-mini", MaxOutput: ptr(1000)}})
	if out[1].Discovered || !out[0].Discovered {
		t.Fatal("only an override that sets the tier vouches for a discovered model")
	}
}
