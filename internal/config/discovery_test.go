package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestFetchProviderModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("wrong auth %q", auth)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{"id": "deepseek-chat", "owned_by": "deepseek"},
				map[string]any{"id": "deepseek-reasoner", "owned_by": "deepseek"},
			},
		})
	}))
	defer upstream.Close()

	f := NewModelsFetcher()
	models, err := f.Fetch(context.Background(), ProviderCfg{
		Name: "deepseek", Type: "openai", BaseURL: upstream.URL + "/v1", APIKey: "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "deepseek-chat" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestManagerListModels(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	defer ts.Close()

	// provider upstream returns discovered models
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{"id": "deepseek-chat"},
				map[string]any{"id": "deepseek-reasoner"},
			},
		})
	}))
	defer upstream.Close()

	seed := Config{
		Providers: []ProviderCfg{
			{Name: "deepseek", Type: "openai", BaseURL: upstream.URL + "/v1", APIKey: "k", Discover: true},
			{Name: "unreachable", Type: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", Discover: true},
		},
		Models: []ModelCfg{
			{Name: "deepseek-chat", Provider: "deepseek", UpstreamModel: "deepseek-chat"}, // 显式，覆盖发现
		},
	}
	for _, p := range seed.Providers {
		js.Put(ctx, ProviderKey(p.Name), p)
	}
	for _, m := range seed.Models {
		js.Put(ctx, ModelKey(m.Name), m)
	}
	mgr := NewManager(js, ts)
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	entries, err := mgr.ListModels(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// 显式 deepseek-chat（无前缀）+ 发现 deepseek/deepseek-reasoner（deepseek-chat 被显式覆盖）
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	if !ids["deepseek-chat"] {
		t.Errorf("explicit model missing: %v", ids)
	}
	if !ids["deepseek/deepseek-reasoner"] {
		t.Errorf("discovered prefixed model missing: %v", ids)
	}
	if ids["deepseek/deepseek-chat"] {
		t.Errorf("discovered duplicate should be covered by explicit model: %v", ids)
	}

	// filter: 单 provider 失败 → 报错
	if _, err := mgr.ListModels(ctx, "unreachable"); err == nil {
		t.Error("filter on failing provider should error")
	}
	// filter: 正常 provider
	got, err := mgr.ListModels(ctx, "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Errorf("filter deepseek should include explicit+discovered: %v", got)
	}
}

func TestManagerListModelsRequiresDiscoveryOptIn(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	defer ts.Close()
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "remote-model"}}})
	}))
	defer upstream.Close()

	provider := ProviderCfg{Name: "zai", Type: "openai", BaseURL: upstream.URL + "/v1"}
	if _, err := js.Put(ctx, ProviderKey(provider.Name), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Put(ctx, ModelKey("manual"), ModelCfg{Name: "manual", Provider: "zai"}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(js, ts)
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := mgr.ListModels(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 || len(entries) != 1 || entries[0].ID != "manual" {
		t.Fatalf("discovery should be opt-in: hits=%d entries=%v", hits, entries)
	}

	provider.Discover = true
	if _, err := js.Put(ctx, ProviderKey(provider.Name), provider); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err = mgr.ListModels(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || len(entries) != 2 {
		t.Fatalf("enabled discovery should fetch remote models: hits=%d entries=%v", hits, entries)
	}
}

func TestBuildPrefixRules(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{
			{Name: "deepseek", Type: "openai", BaseURL: "http://x/v1", Discover: true},
			{Name: "openrouter", Type: "openai", BaseURL: "http://o/v1", Discover: true},
		},
	}
	rt, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rt.Table.All() {
		for _, c := range r.Match.Conds {
			if c.Field == "model" && c.Op == "prefix" {
				got[c.Value.(string)] = true
			}
		}
	}
	if !got["deepseek/"] || !got["openrouter/"] {
		t.Errorf("openai providers should get discovery prefix rules: %v", got)
	}
}
