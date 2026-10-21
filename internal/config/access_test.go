package config_test

import (
	"context"
	"testing"

	"lmgateway/internal/auth"
	"lmgateway/internal/config"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
	"lmgateway/internal/store/mem"
)

func publicPtr(v bool) *bool { return &v }

func serveWithGroups(t *testing.T, rt *config.Runtime, model string, groups []string) packet.Packet {
	t.Helper()
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	pkt.Set(packet.KeyPrincipal, map[string]any{"id": "k1", "kind": "apikey", "groups": groups})
	return rt.Dispatcher.Serve(pkt, packet.SourceIngress)
}

func TestBuildRegistersAuthorizer(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://127.0.0.1:1"}},
		Models: []config.ModelCfg{
			{Name: "gated", Provider: "p", AllowedGroups: []string{"eng"}},
			{Name: "internal", Provider: "p", Public: publicPtr(false)},
		},
	}
	rt, err := config.Build(cfg, config.BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Registry.Has("authorize") {
		t.Fatal("authorize handler should be registered")
	}

	// denied: caller has no group matching the model ACL
	out := serveWithGroups(t, rt, "gated", nil)
	if kind, _ := out.ErrorKind(); kind != packet.ErrPolicy {
		t.Fatalf("expected policy denial, got %q", kind)
	}
	if status, _ := out.ErrorStatus(); status != 403 {
		t.Fatalf("expected 403, got %d", status)
	}
}

func TestInternalModelNotAddressable(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://127.0.0.1:1"}},
		Models: []config.ModelCfg{
			{Name: "internal", Provider: "p", Public: publicPtr(false)},
		},
	}
	rt, err := config.Build(cfg, config.BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// exists in routing but is not a public name -> 403
	out := serveWithGroups(t, rt, "internal", []string{"anything"})
	if kind, _ := out.ErrorKind(); kind != packet.ErrPolicy {
		t.Fatalf("internal model should be forbidden, got %q", kind)
	}
}

func TestUnknownModelFallsThrough(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://127.0.0.1:1"}},
		Models:    []config.ModelCfg{{Name: "known", Provider: "p"}},
	}
	rt, err := config.Build(cfg, config.BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// unknown name -> not a public name, not routable -> no_route (404), not 403
	out := serveWithGroups(t, rt, "totally-unknown", nil)
	if kind, _ := out.ErrorKind(); kind == packet.ErrPolicy {
		t.Fatalf("unknown model should not be a policy denial")
	}
}

func TestAliasIsPublicEntry(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://127.0.0.1:1"}},
		Models:    []config.ModelCfg{{Name: "gpt-5", Provider: "p"}},
		Rules: []config.RuleCfg{{
			ID: "alias-agent", From: "http",
			Match:         []config.ConditionCfg{{Field: "model", Op: "eq", Value: "agent"}},
			Set:           map[string]any{"model": "gpt-5"},
			To:            "http",
			AllowedGroups: []string{"ops"},
		}},
	}
	rt, err := config.Build(cfg, config.BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// caller without ops is denied at the alias name
	out := serveWithGroups(t, rt, "agent", []string{"eng"})
	if kind, _ := out.ErrorKind(); kind != packet.ErrPolicy {
		t.Fatalf("alias should be gated, got %q", kind)
	}
}

func TestKeyAndNamespaceStoreResolution(t *testing.T) {
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
	if len(cfg.Keys) != 1 || cfg.Keys[0].ID != "k1" {
		t.Fatalf("baseline key not resolved: %+v", cfg.Keys)
	}
	// sanity: auth package hash helper is deterministic
	if auth.HashToken("x") == auth.HashToken("y") {
		t.Fatal("hash collision sanity")
	}
}
