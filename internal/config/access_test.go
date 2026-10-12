package config_test

import (
	"context"
	"testing"

	"lmgateway/internal/config"
	"lmgateway/internal/store/mem"
)

func TestKeyStoreResolution(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x"}},
		Models:    []config.ModelCfg{{Name: "m", Provider: "p"}},
		Keys:      []config.KeyCfg{{ID: "k1", KeyHash: "hash1", Groups: []string{"eng"}}},
	}
	if err := config.ReconcileBaseline(ctx, js, base); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Keys) != 1 || cfg.Keys[0].ID != "k1" || len(cfg.Keys[0].Groups) != 1 {
		t.Fatalf("baseline key not resolved: %+v", cfg.Keys)
	}
	// DB override replaces the baseline key.
	if _, err := js.Put(ctx, config.KeyKey("k1"), config.KeyCfg{ID: "k1", KeyHash: "hash2", Groups: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Keys) != 1 || cfg.Keys[0].KeyHash != "hash2" || cfg.Keys[0].Groups[0] != "ops" {
		t.Fatalf("db override not applied: %+v", cfg.Keys)
	}
}

func TestBuildRegistersAccessGuards(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x", AllowedGroups: []string{"eng"}}},
		Models: []config.ModelCfg{
			{Name: "gated", Provider: "p", AllowedGroups: []string{"eng"}},
			{Name: "open", Provider: "p"},
		},
	}
	rt, err := config.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Registry.Has("access:model:gated") {
		t.Fatal("gated model should have an access guard")
	}
	// 'open' has no ACL of its own but inherits the provider ACL
	if !rt.Registry.Has("access:model:open") {
		t.Fatal("model without own ACL should inherit provider ACL")
	}
	// the provider prefix route is guarded too
	if !rt.Registry.Has("access:provider:p") {
		t.Fatal("provider prefix route should be guarded")
	}
}
