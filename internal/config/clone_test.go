package config_test

import (
	"context"
	"testing"

	"lmgateway/internal/config"
	"lmgateway/internal/store/mem"
)

func TestConfigFromStoreWithBaseDoesNotMutateBase(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := config.Config{Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://yaml"}}}
	if _, err := js.Put(ctx, config.ProviderKey("openai"), config.ProviderCfg{Name: "openai", Type: "openai", BaseURL: "http://override"}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ConfigFromStoreWithBase(ctx, js, base); err != nil {
		t.Fatal(err)
	}
	if base.Providers[0].BaseURL != "http://yaml" {
		t.Fatalf("base config was mutated: %+v", base)
	}
}
