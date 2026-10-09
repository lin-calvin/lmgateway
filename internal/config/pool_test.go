package config_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lmgateway/internal/config"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestPoolStoreResolution(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := config.Config{
		Pools: []config.PoolCfg{{Model: "p", Backend: []string{"a/m", "b/m"}}},
	}
	if err := config.ReconcileBaseline(ctx, js, base); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pools) != 1 || cfg.Pools[0].Model != "p" || len(cfg.Pools[0].Backend) != 2 {
		t.Fatalf("baseline pool not resolved: %+v", cfg.Pools)
	}

	// DB override wins over the YAML baseline.
	if _, err := js.Put(ctx, config.PoolKey("p"), config.PoolCfg{Model: "p", Backend: []string{"c/m"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pools) != 1 || len(cfg.Pools[0].Backend) != 1 || cfg.Pools[0].Backend[0] != "c/m" {
		t.Fatalf("db override not applied: %+v", cfg.Pools)
	}
}

func TestPrefixRouteWithoutDiscovery(t *testing.T) {
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "created": 0, "model": gotModel,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer server.Close()

	// provider without discovery must still route [provider]/model.
	cfg := config.Config{
		Providers: []config.ProviderCfg{{Name: "autodl", Type: "openai", BaseURL: server.URL}},
	}
	rt, err := config.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":    "autodl/Some-Model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := rt.Dispatcher.Serve(pkt, packet.SourceHTTP)
	if msg, ok := out.Error(); ok {
		t.Fatalf("expected prefix route without discovery, got error: %v", msg)
	}
	if gotModel != "Some-Model" {
		t.Fatalf("expected upstream model stripped to Some-Model, got %q", gotModel)
	}
}

func TestPoolRotatesOnRateLimit(t *testing.T) {
	var aCalls, bCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a/chat/completions":
			aCalls++
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		case "/b/chat/completions":
			bCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "x", "object": "chat.completion", "created": 0, "model": "backend",
				"choices": []any{map[string]any{
					"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	m := config.NewManager(js, ts)
	cfg := config.Config{
		Providers: []config.ProviderCfg{
			{Name: "pa", Type: "openai", BaseURL: server.URL + "/a"},
			{Name: "pb", Type: "openai", BaseURL: server.URL + "/b"},
		},
		Models: []config.ModelCfg{
			{Name: "pa/m", Provider: "pa"},
			{Name: "pb/m", Provider: "pb"},
		},
		Pools: []config.PoolCfg{{Model: "pooled", Backend: []string{"pa/m", "pb/m"}, CooldownSec: 60}},
	}
	if err := m.SetBase(cfg); err != nil {
		t.Fatal(err)
	}

	request := func() packet.Packet {
		doc, err := lm.NewRequest("openai", map[string]any{
			"model": "pooled",
			"messages": []any{map[string]any{
				"role": "user", "content": "hi",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		pkt := packet.NewReqDocument(doc)
		pkt.Set(packet.KeyCtx, context.Background())
		// fetch the current dispatcher each time, like httpapi does
		return m.Dispatcher().Serve(pkt, packet.SourceIngress)
	}

	// First request: the pool alias rule points at pa/m, which rate-limits.
	first := request()
	firstMsg, firstErr := first.Error()
	if class, _ := first.ErrorClass(); class != packet.ClassRateLimit {
		t.Fatalf("expected rate_limit from backend a, got %q (err=%v)", class, firstMsg)
	}
	if !firstErr {
		t.Fatalf("expected an error packet from backend a")
	}
	if aCalls != 1 {
		t.Fatalf("backend a should have been called once, got %d", aCalls)
	}
	// The passive reporter rotated the control plane; a routing recompile makes
	// the alias rule point at pb/m.
	if active, _ := m.Pools().Active("pooled"); active != "pb/m" {
		t.Fatalf("expected pool rotated to pb/m, got %q", active)
	}
	if err := m.Recompile(); err != nil {
		t.Fatal(err)
	}

	second := request()
	if msg, ok := second.Error(); ok {
		t.Fatalf("second request should succeed on backend b: %v", msg)
	}
	if bCalls != 1 {
		t.Fatalf("backend b should have served the second request, got %d", bCalls)
	}
	resp, _ := second.Response()
	if content, _ := resp.Get("choices"); content == nil {
		t.Fatalf("missing choices in response")
	}
}
