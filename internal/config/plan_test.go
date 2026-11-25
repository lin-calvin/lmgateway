package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolvePlanPreset(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{{
			Name:    "opencode_go",
			Type:    "openai",
			BaseURL: "http://x",
			Plan: &PlanCfg{
				Type: "opencode_go",
				ModelPrices: map[string]PlanModelPrice{
					"gpt-5.6": {InputPerMtok: 3, OutputPerMtok: 4},
				},
			},
		}},
		Models: []ModelCfg{{Name: "opencode_go/gpt-5.6", Provider: "opencode_go"}},
	}
	rt, err := Build(cfg, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	plan := rt.Config.Providers[0].Plan
	if plan == nil {
		t.Fatal("plan not resolved")
	}
	if plan.Price != 10 || plan.CostModel != "multiplier" || plan.RateLimitFamily != "opencode" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Multipliers["default"] != 1 {
		t.Fatalf("multipliers = %v", plan.Multipliers)
	}
	model := rt.Config.Models[0]
	if model.InputPerMtok != 3 || model.OutputPerMtok != 4 {
		t.Fatalf("model price = %v/%v", model.InputPerMtok, model.OutputPerMtok)
	}
	// The caller's config must not be mutated by preset resolution.
	if cfg.Models[0].InputPerMtok != 0 || cfg.Providers[0].Plan.Price != 0 {
		t.Fatalf("caller config mutated: %+v", cfg)
	}
}

func TestGeneratedOpenCodePreset(t *testing.T) {
	preset, ok := PlanPresets()["opencode_go"]
	if !ok {
		t.Fatal("opencode_go preset missing")
	}
	if preset.Price != 10 || preset.CostModel != "multiplier" {
		t.Fatalf("preset = %+v", preset)
	}
	if len(preset.Models) == 0 {
		t.Fatal("opencode_go preset has no model prices")
	}
	priced := false
	for _, price := range preset.Models {
		if price.InputPerMtok > 0 || price.OutputPerMtok > 0 {
			priced = true
			break
		}
	}
	if !priced {
		t.Fatal("opencode_go preset has no non-zero model prices")
	}
	if got := preset.Multipliers["deepseek-v4.1-flash"]; got != 0.1 {
		t.Fatalf("opencode_go multiplier = %v, want 0.1", got)
	}
	if got := preset.Models["deepseek-v4.1-flash"].CachedInputPerMtok; got != 0.003 {
		t.Fatalf("opencode_go cached price = %v, want 0.003", got)
	}
	plus := PlanPresets()["opencode_go_plus"]
	if got := plus.Multipliers["deepseek-v4.1-flash"]; got != 0.2 {
		t.Fatalf("opencode_go_plus multiplier = %v, want 0.2", got)
	}
}

func TestResolvePlanUnknownPreset(t *testing.T) {
	cfg := Config{Providers: []ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x", Plan: &PlanCfg{Type: "nope"}}}}
	if _, err := Build(cfg, BuildDeps{}); err == nil {
		t.Fatal("expected unknown preset error")
	}
}

func TestPlanCfgStringSugar(t *testing.T) {
	var y struct {
		Plan *PlanCfg `yaml:"plan"`
	}
	if err := yaml.Unmarshal([]byte("plan: opencode_go\n"), &y); err != nil {
		t.Fatal(err)
	}
	if y.Plan == nil || y.Plan.Type != "opencode_go" {
		t.Fatalf("yaml plan = %+v", y.Plan)
	}

	var j struct {
		Plan *PlanCfg `json:"plan"`
	}
	if err := json.Unmarshal([]byte(`{"plan":"chatgpt_plus"}`), &j); err != nil {
		t.Fatal(err)
	}
	if j.Plan == nil || j.Plan.Type != "chatgpt_plus" {
		t.Fatalf("json plan = %+v", j.Plan)
	}
}

func TestPricingLookupProviderScopedAndPlanFallback(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{
			{Name: "a", Type: "openai", BaseURL: "http://a", Plan: &PlanCfg{Type: "opencode_go"}},
			{Name: "b", Type: "openai", BaseURL: "http://b"},
		},
		Models: []ModelCfg{
			{Name: "m", Provider: "a", InputPerMtok: 1, OutputPerMtok: 2},
			{Name: "m", Provider: "b", InputPerMtok: 10, OutputPerMtok: 20},
		},
	}
	m := NewManagerWithBase(nil, nil, Config{})
	if err := m.Build(cfg); err != nil {
		t.Fatal(err)
	}
	lookup := m.pricingLookup()
	if p, ok := lookup("a", "m"); !ok || p.InputPerMtok != 1 || p.OutputPerMtok != 2 {
		t.Fatalf("a/m = %+v ok=%v", p, ok)
	}
	if p, ok := lookup("b", "m"); !ok || p.InputPerMtok != 10 || p.OutputPerMtok != 20 {
		t.Fatalf("b/m = %+v ok=%v", p, ok)
	}
	if p, ok := lookup("a", "deepseek-v4.1-flash"); !ok || p.InputPerMtok != 0.15 || p.CachedInputPerMtok != 0.003 || p.OutputPerMtok != 0.6 {
		t.Fatalf("plan fallback = %+v ok=%v", p, ok)
	}
}

func TestEffectiveModelPriceAppliesPlanMultiplier(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{{
			Name:    "go",
			Type:    "openai",
			BaseURL: "http://x",
			Plan:    &PlanCfg{Type: "opencode_go"},
		}},
		Models: []ModelCfg{{Name: "deepseek-v4.1-flash", Provider: "go"}},
	}
	rt, err := Build(cfg, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// Catalog output 0.6 × (10 / 100) = 0.06.
	if got := effectiveModelPrice(rt.Config, rt.Config.Models[0]); got != 0.06 {
		t.Fatalf("effective price = %v, want 0.06", got)
	}

	plus := cfg
	plus.Providers = []ProviderCfg{{
		Name:    "go",
		Type:    "openai",
		BaseURL: "http://x",
		Plan:    &PlanCfg{Type: "opencode_go_plus"},
	}}
	rtPlus, err := Build(plus, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// Catalog output 0.6 × (40 / 200) = 0.12.
	if got := effectiveModelPrice(rtPlus.Config, rtPlus.Config.Models[0]); got != 0.12 {
		t.Fatalf("plus effective price = %v, want 0.12", got)
	}

	plain := Config{
		Providers: []ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x"}},
		Models:    []ModelCfg{{Name: "m", Provider: "p", OutputPerMtok: 2}},
	}
	rtPlain, err := Build(plain, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveModelPrice(rtPlain.Config, rtPlain.Config.Models[0]); got != 2 {
		t.Fatalf("plain price = %v, want 2", got)
	}
}

func TestBuildSources(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{
			{Name: "go", Type: "openai", BaseURL: "http://x", Plan: &PlanCfg{Type: "opencode_go"}},
			{Name: "codex", Type: "openai", BaseURL: "http://x", Plan: &PlanCfg{Type: "chatgpt_plus"}},
			{Name: "generic", Type: "openai", BaseURL: "http://x", Plan: &PlanCfg{CostModel: "multiplier"}},
		},
	}
	rt, err := Build(cfg, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Sources) != 2 {
		t.Fatalf("sources = %+v", rt.Sources)
	}
	if rt.Sources["go"].Timer == nil || rt.Sources["go"].Sink != nil {
		t.Fatalf("opencode source = %+v", rt.Sources["go"])
	}
	if rt.Sources["codex"].Sink == nil || rt.Sources["codex"].Timer != nil {
		t.Fatalf("codex source = %+v", rt.Sources["codex"])
	}
	if _, ok := rt.Sources["generic"]; ok {
		t.Fatalf("generic provider should use the parser fallback: %+v", rt.Sources)
	}
}

func TestProviderTypeDialectAlias(t *testing.T) {
	cfg := Config{Providers: []ProviderCfg{{Name: "deepseek", Type: "deepseek", BaseURL: "http://x"}}}
	rt, err := Build(cfg, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	provider := rt.Config.Providers[0]
	if provider.Type != "openai" || provider.Dialect != "deepseek" {
		t.Fatalf("provider = %+v", provider)
	}
	explicit := Config{Providers: []ProviderCfg{{Name: "p", Type: "glm", Dialect: "qwen", BaseURL: "http://x"}}}
	rt, err = Build(explicit, BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Config.Providers[0].Type != "openai" || rt.Config.Providers[0].Dialect != "qwen" {
		t.Fatalf("explicit provider = %+v", rt.Config.Providers[0])
	}
}
