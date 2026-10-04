package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lmgateway/internal/api"
	"lmgateway/internal/config"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestPoolStatusRotateClear(t *testing.T) {
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	seed := config.Config{
		Providers: []config.ProviderCfg{
			{Name: "pa", Type: "openai", BaseURL: "http://x"},
			{Name: "pb", Type: "openai", BaseURL: "http://x"},
		},
		Models: []config.ModelCfg{
			{Name: "pa/m", Provider: "pa"},
			{Name: "pb/m", Provider: "pb"},
		},
		Pools: []config.PoolCfg{{Model: "pooled", Backend: []string{"pa/m", "pb/m"}, CooldownSec: 60}},
	}
	m := config.NewManagerWithBase(js, ts, seed)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Deps{Manager: m, TS: ts, Rollup: rollup.New(ts, js), Pools: m.Pools()}))
	t.Cleanup(func() { srv.Close(); ts.Close(); m.Close() })

	decode := func(resp *http.Response) map[string]any {
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return out
	}

	resp := do(t, http.MethodGet, srv.URL+"/api/pool/status/pooled", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if status := decode(resp); status["active"] != "pa/m" {
		t.Fatalf("initial active = %v", status["active"])
	}

	resp = do(t, http.MethodPost, srv.URL+"/api/pool/rotate/pooled", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate code %d", resp.StatusCode)
	}
	if out := decode(resp); out["active"] != "pb/m" {
		t.Fatalf("rotated active = %v", out["active"])
	}

	resp = do(t, http.MethodPost, srv.URL+"/api/pool/clear/pooled", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear code %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, srv.URL+"/api/pool/status/nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown pool, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
