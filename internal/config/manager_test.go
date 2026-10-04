package config_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func setupManager(t *testing.T) (*config.Manager, store.JSONStore, store.TSStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 1000, Interval: time.Hour})

	seed := config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x", APIKey: "k"}},
		Models:    []config.ModelCfg{{Name: "gpt-4o", Provider: "openai"}},
		Rules: []config.RuleCfg{
			{ID: "r1", From: "http", Action: "openai", Match: []config.ConditionCfg{
				{Field: "metadata.internal", Op: "eq", Value: true},
			}},
		},
	}
	for _, p := range seed.Providers {
		if _, err := js.Put(ctx, config.ProviderKey(p.Name), p); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range seed.Models {
		if _, err := js.Put(ctx, config.ModelKey(m.Name), m); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range seed.Rules {
		if _, err := js.Put(ctx, config.RuleKey(r.ID), r); err != nil {
			t.Fatal(err)
		}
	}

	m := config.NewManager(js, ts)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return m, js, ts, ctx
}

func TestLoadServerCfg(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("server:\n  addr: :8080\n  master_key: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := config.LoadServerCfg(path)
	if err != nil {
		t.Fatal(err)
	}
	if server.Addr != ":8080" || server.MasterKey != "secret" {
		t.Fatalf("unexpected server config: %+v", server)
	}
}

func TestHotReload(t *testing.T) {
	m, js, _, ctx := setupManager(t)

	if got := len(m.Runtime().Config.Providers); got != 1 {
		t.Fatalf("expected 1 provider initially, got %d", got)
	}

	// 热写：新增 provider + model → watch 触发 reload → 运行配置更新
	if _, err := js.Put(ctx, config.ProviderKey("azure"),
		config.ProviderCfg{Name: "azure", Type: "openai", BaseURL: "http://a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Put(ctx, config.ModelKey("claude"),
		config.ModelCfg{Name: "claude", Provider: "azure"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(m.Runtime().Config.Providers) == 2 })
	if got := len(m.Runtime().Config.Providers); got != 2 {
		t.Fatalf("hot reload did not pick up new provider, got %d", got)
	}
	if got := len(m.Runtime().Config.Models); got != 2 {
		t.Fatalf("expected 2 models after reload, got %d", got)
	}
}

// 坏配置写入不应破坏运行中的配置（reload 失败保持旧 Runtime）
func TestHotReloadKeepsOldOnError(t *testing.T) {
	m, js, _, ctx := setupManager(t)

	// 写一条 action 未注册的规则 → Build 失败 → 运行配置不变
	if _, err := js.Put(ctx, config.RuleKey("bad"),
		config.RuleCfg{ID: "bad", From: "http", Action: "no-such-handler"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 等 watch + 失败的 reload
	if got := len(m.Runtime().Config.Providers); got != 1 {
		t.Errorf("bad config should not be applied, providers=%d", got)
	}
	if got := len(m.Runtime().Config.Rules); got != 1 {
		t.Errorf("bad rule should not be applied, rules=%d", got)
	}
}

func TestSeedFromFileDoesNotPersistBaseline(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	path := t.TempDir() + "/seed.yaml"
	if err := os.WriteFile(path, []byte("providers:\n  - name: base\n    type: openai\n    base_url: http://base\nmodels:\n  - name: base-model\n    provider: base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SeedFromFile(ctx, js, path); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{config.ProviderKey("base"), config.ModelKey("base-model")} {
		doc, err := js.Get(ctx, key)
		if err != nil {
			t.Fatalf("baseline marker %s should be persisted: %v", key, err)
		}
		if string(doc.Data) != `{"source":"yaml"}` {
			t.Fatalf("unexpected baseline marker %s: %s", key, doc.Data)
		}
	}
}

func TestConfigFromStoreWithBaseMergesOverridesAndDatabaseOnly(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := config.Config{
		Providers: []config.ProviderCfg{{Name: "base", Type: "openai", BaseURL: "http://base"}},
		Models:    []config.ModelCfg{{Name: "base-model", Provider: "base"}},
	}
	if _, err := js.Put(ctx, config.ProviderKey("base"), config.ProviderCfg{Name: "base", Type: "openai", BaseURL: "http://override"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Put(ctx, config.ProviderKey("db-only"), config.ProviderCfg{Name: "db-only", Type: "openai", BaseURL: "http://db-only"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].BaseURL != "http://override" {
		t.Fatalf("unexpected merged providers: %+v", cfg.Providers)
	}
	if cfg.Providers[1].Name != "db-only" {
		t.Fatalf("database-only provider was not retained: %+v", cfg.Providers)
	}
}

func TestDefaultModelIsInjected(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	seed := config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Models:    []config.ModelCfg{{Name: "default-model", Provider: "openai", Default: true}},
	}
	path := t.TempDir() + "/seed.yaml"
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SeedFromFile(ctx, js, path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ConfigFromStoreWithBase(ctx, js, seed)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := config.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range rt.Table.All() {
		if rule.From == "http" && rule.Action == "openai" && rule.Set["model"] == "default-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("default route should inject the default model")
	}
}

func TestRuleDefaultCompilesAndPureDefaultAllowed(t *testing.T) {
	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Models:    []config.ModelCfg{{Name: "m", Provider: "openai"}},
		Rules: []config.RuleCfg{{
			ID:      "default-reasoning",
			From:    "http",
			Action:  "openai",
			Match:   []config.ConditionCfg{{Field: "model", Op: "eq", Value: "m"}},
			Set:     map[string]any{"model": "m"},
			Default: map[string]any{"reasoning.effort": "max"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range rt.Table.All() {
		if rule.ID == "default-reasoning" && rule.Default["reasoning.effort"] == "max" && rule.Set["model"] == "m" {
			found = true
		}
	}
	if !found {
		t.Fatal("rule default was not compiled into the table")
	}

	if _, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Rules:     []config.RuleCfg{{ID: "pure", From: "http", To: "http", Default: map[string]any{"reasoning.effort": "max"}}},
	}); err != nil {
		t.Fatalf("pure default rule should compile: %v", err)
	}
}

func TestSeedOverlayLeavesOmittedSettingsUntouched(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	if _, err := js.Put(ctx, config.ServerSettingKey, config.ServerCfg{Addr: ":9999"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Put(ctx, config.SpendSettingKey, config.SpendCfg{RawRetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/seed.yaml"
	if err := os.WriteFile(path, []byte("providers: []\nmodels: []\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SeedFromFile(ctx, js, path); err != nil {
		t.Fatal(err)
	}
	server, err := js.Get(ctx, config.ServerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(server.Data) != `{"addr":":9999"}` && string(server.Data) != `{"addr":":9999","source":"db_override"}` {
		t.Fatalf("omitted server setting was overwritten: %s", server.Data)
	}
	spend, err := js.Get(ctx, config.SpendSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var spendCfg config.SpendCfg
	if err := json.Unmarshal(spend.Data, &spendCfg); err != nil {
		t.Fatal(err)
	}
	if spendCfg.RawRetentionDays != 30 {
		t.Fatalf("omitted spend setting was overwritten: %s", spend.Data)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
