package config_test

import (
	"context"
	"encoding/json"
	"testing"

	"lmgateway/internal/config"
	"lmgateway/internal/store/mem"
)

func TestConfigItemSources(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := config.Config{Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://yaml"}}}
	if _, err := js.Put(ctx, config.ProviderKey("openai"), map[string]any{"source": config.SourceYAML}); err != nil {
		t.Fatal(err)
	}
	item, err := config.ResolveItem(ctx, js, base, config.ProviderKey("openai"))
	if err != nil {
		t.Fatal(err)
	}
	var yamlValue config.ProviderCfg
	if err := json.Unmarshal(item.Data, &yamlValue); err != nil {
		t.Fatal(err)
	}
	if item.Source != config.SourceYAML || yamlValue.BaseURL != "http://yaml" {
		t.Fatalf("unexpected yaml item: %+v %s", item, item.Data)
	}
	if _, err := js.Put(ctx, config.ProviderKey("openai"), map[string]any{"source": config.SourceDBOverride, "name": "openai", "type": "openai", "base_url": "http://db"}); err != nil {
		t.Fatal(err)
	}
	item, err = config.ResolveItem(ctx, js, base, config.ProviderKey("openai"))
	if err != nil {
		t.Fatal(err)
	}
	var value config.ProviderCfg
	if err := json.Unmarshal(item.Data, &value); err != nil {
		t.Fatal(err)
	}
	if item.Source != config.SourceDBOverride || value.BaseURL != "http://db" {
		t.Fatalf("unexpected override item: %+v %s", item, item.Data)
	}
}
