package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"lmgateway/internal/api"
	"lmgateway/internal/config"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func setup(t *testing.T) (*config.Manager, store.TSStore, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 1000, Interval: time.Hour})
	seed := config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Models:    []config.ModelCfg{{Name: "gpt-4o", Provider: "openai"}},
	}
	m := config.NewManagerWithBase(js, ts, seed)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Deps{Manager: m, TS: ts, Rollup: rollup.New(ts, js)}))
	t.Cleanup(func() { srv.Close(); ts.Close(); m.Close() })
	return m, ts, srv
}

func do(t *testing.T, method, url string, body any, hdr map[string]string) *http.Response {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestProviderCRUD(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/provider/set/azure", map[string]any{"type": "openai", "base_url": "http://a", "api_key": "secret"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set expected 200, got %d", resp.StatusCode)
	}
	var set struct {
		Version int64 `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&set)
	resp.Body.Close()
	if set.Version < 1 {
		t.Error("expected version")
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/provider/get/azure", nil, nil)
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["api_key"] != "****" || got["source"] != "db" {
		t.Errorf("unexpected provider: %v", got)
	}
	resp = do(t, http.MethodPut, srv.URL+"/api/provider/set/bad", map[string]any{"type": "bogus", "base_url": "http://a"}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad type expected 422, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodDelete, srv.URL+"/api/provider/delete/azure", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConfigSourcesAndReset(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodGet, srv.URL+"/api/provider/get/openai", nil, nil)
	var provider map[string]any
	json.NewDecoder(resp.Body).Decode(&provider)
	resp.Body.Close()
	if provider["source"] != "yaml" {
		t.Fatalf("expected yaml source, got %v", provider["source"])
	}
	resp = do(t, http.MethodPut, srv.URL+"/api/provider/set/openai", map[string]any{"type": "openai", "base_url": "http://override"}, nil)
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || result["source"] != "db_override" {
		t.Fatalf("unexpected override response: %d %v", resp.StatusCode, result)
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/provider/get/openai", nil, nil)
	json.NewDecoder(resp.Body).Decode(&provider)
	resp.Body.Close()
	if provider["source"] != "db_override" || provider["base_url"] != "http://override" {
		t.Fatalf("unexpected override provider: %v", provider)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/provider/reset/openai", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset expected 200, got %d", resp.StatusCode)
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/provider/get/openai", nil, nil)
	var resetProvider map[string]any
	json.NewDecoder(resp.Body).Decode(&resetProvider)
	resp.Body.Close()
	if resetProvider["source"] != "yaml" || resetProvider["base_url"] != "http://x" {
		t.Fatalf("unexpected reset provider: %v", resetProvider)
	}
}

func TestSchemaValidation(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/provider/set/x", map[string]any{"type": "openai"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing base_url expected 422, got %d", resp.StatusCode)
	}
}

func TestModelSemanticValidation(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/model/set/foo", map[string]any{"provider": "nonexistent"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown provider expected 422, got %d", resp.StatusCode)
	}
}

func TestRuleCRUDAndMeta(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/config/rules/set/r1", map[string]any{"from": "http", "action": "openai", "match": []any{map[string]any{"field": "metadata.internal", "op": "eq", "value": true}}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set rule expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodGet, srv.URL+"/api/config/rules/get/r1", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get rule expected 200, got %d", resp.StatusCode)
	}
	resp = do(t, http.MethodPut, srv.URL+"/api/config/rules/set/r2", map[string]any{"from": "http", "action": "openai", "match": []any{map[string]any{"field": "req.modle", "op": "eq", "value": "x"}}}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("typo field expected 422, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodGet, srv.URL+"/api/config/rules/meta", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta expected 200, got %d", resp.StatusCode)
	}
}

func TestSettingsAndAction(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/config/settings/set/spend", map[string]any{"raw_retention_days": 3, "rollup_dimensions": []string{"provider", "model"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set spend expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodGet, srv.URL+"/api/config/settings/get/spend", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get spend expected 200, got %d", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, srv.URL+"/api/action/config/reload", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload expected 200, got %d", resp.StatusCode)
	}
}

func TestOptimisticLock(t *testing.T) {
	_, _, srv := setup(t)
	resp := do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o", map[string]any{"provider": "openai"}, nil)
	var set struct {
		Version int64 `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&set)
	resp.Body.Close()
	if set.Version == 0 {
		t.Fatal("expected persisted override version")
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/model/get/gpt-4o", nil, nil)
	var got struct {
		Version int64 `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	resp = do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o", map[string]any{"provider": "openai"}, map[string]string{"If-Match": "999"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale If-Match expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o", map[string]any{"provider": "openai"}, map[string]string{"If-Match": strconv.FormatInt(got.Version, 10)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct If-Match expected 200, got %d", resp.StatusCode)
	}
}

func TestTSAggRead(t *testing.T) {
	_, ts, srv := setup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, record := range []*store.TSRecord{
		{Ts: now.Add(-2 * time.Hour), Stream: "spend", Tags: map[string]string{"provider": "openai", "model": "gpt-4o"}, Fields: map[string]any{"total_tokens": 100.0, "prompt_tokens": 40.0, "completion_tokens": 60.0}},
		{Ts: now.Add(-time.Hour), Stream: "spend", Tags: map[string]string{"provider": "openai", "model": "gpt-4o"}, Fields: map[string]any{"total_tokens": 200.0, "prompt_tokens": 80.0, "completion_tokens": 120.0}},
	} {
		if err := ts.Append(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	flush, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ts.Flush(flush); err != nil {
		t.Fatal(err)
	}

	readSum := func(path string) float64 {
		t.Helper()
		resp := do(t, http.MethodGet, srv.URL+path, nil, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("agg expected 200, got %d", resp.StatusCode)
		}
		var out struct {
			Rows []struct {
				Sums map[string]float64 `json:"Sums"`
			} `json:"rows"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if len(out.Rows) != 1 {
			t.Fatalf("expected one aggregation row, got %+v", out.Rows)
		}
		return out.Rows[0].Sums["total_tokens"]
	}
	from := now.Add(-90 * time.Minute).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	if got := readSum("/api/ts/spend/agg?group=model&from=" + from + "&to=" + to); got != 200 {
		t.Fatalf("time range should include only newer record, got %v", got)
	}
	resp := do(t, http.MethodGet, srv.URL+"/api/ts/spend/agg?to=2026-09-9T00:00:00Z", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid RFC3339 to expected 400, got %d", resp.StatusCode)
	}
	resp = do(t, http.MethodGet, srv.URL+"/api/ts/spend/agg?from="+to+"&to="+from, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reversed time range expected 400, got %d", resp.StatusCode)
	}
}

func TestTokenToday(t *testing.T) {
	_, ts, srv := setup(t)
	ctx := context.Background()
	ts.Append(ctx, &store.TSRecord{Ts: time.Now().UTC(), Stream: "spend", Tags: map[string]string{"provider": "openai", "model": "gpt-4o", "stream": "true", "status": "ok"}, Fields: map[string]any{"prompt_tokens": 40.0, "completion_tokens": 20.0, "total_tokens": 60.0, "latency_ms": 1000.0, "ttft_ms": 250.0, "ttft_samples": 1.0, "generation_ms": 750.0, "generation_samples": 1.0, "generation_tokens": 20.0}})
	flush, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ts.Flush(flush); err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodGet, srv.URL+"/api/spend/token?time=today", nil, nil)
	defer resp.Body.Close()
	var out struct {
		Timing struct {
			TTFT struct {
				Sum float64 `json:"sum"`
			} `json:"ttft_ms"`
			Generation struct {
				Sum float64 `json:"sum"`
			} `json:"generation_ms"`
			GenerationTokensPerSecond float64 `json:"generation_tokens_per_second"`
		} `json:"timing_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Timing.TTFT.Sum != 250 || out.Timing.Generation.Sum != 750 || out.Timing.GenerationTokensPerSecond != 26.666666666666668 {
		t.Fatalf("unexpected timing summary: %+v", out.Timing)
	}
}

// TestDisplaySetting 覆盖"汇率由服务端下发"：写入、读回、非法值明确拒绝。
func TestDisplaySetting(t *testing.T) {
	_, _, srv := setup(t)

	// 写入展示设置
	resp := do(t, http.MethodPut, srv.URL+"/api/config/settings/set/display",
		map[string]any{"currency": "CNY", "usd_to_cny": 7.35}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set display expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	readDisplay := func() map[string]any {
		t.Helper()
		r := do(t, http.MethodGet, srv.URL+"/api/config/settings/get/display", nil, nil)
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("get display expected 200, got %d", r.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := readDisplay()
	if got["currency"] != "CNY" {
		t.Fatalf("currency not persisted: %+v", got)
	}
	if rate, _ := got["usd_to_cny"].(float64); rate != 7.35 {
		t.Fatalf("usd_to_cny not persisted: %+v", got)
	}

	// 不支持的币种 → 422（而不是静默回落，否则"改了汇率却没生效"极难排查）
	resp = do(t, http.MethodPut, srv.URL+"/api/config/settings/set/display",
		map[string]any{"currency": "EUR", "usd_to_cny": 7.35}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported currency expected 422, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 非法汇率 → 422
	resp = do(t, http.MethodPut, srv.URL+"/api/config/settings/set/display",
		map[string]any{"currency": "CNY", "usd_to_cny": -1}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("negative rate expected 422, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 被拒的写入不能改动已存的值
	if again := readDisplay(); again["currency"] != "CNY" {
		t.Fatalf("rejected write must not modify stored value: %+v", again)
	}
}

// TestModelCachedPriceRoundTrip 模型实体要能存缓存价。
// JSON Schema 由 DocType 生成，如果新字段没被 schema 接受，写入会被 422 拒掉。
func TestModelCachedPriceRoundTrip(t *testing.T) {
	_, _, srv := setup(t)

	resp := do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o",
		map[string]any{"provider": "openai", "input_per_mtok": 2.5, "output_per_mtok": 10, "cached_input_per_mtok": 1.25}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set model expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	r := do(t, http.MethodGet, srv.URL+"/api/model/get/gpt-4o", nil, nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get model expected 200, got %d", r.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if rate, _ := out["cached_input_per_mtok"].(float64); rate != 1.25 {
		t.Fatalf("cached_input_per_mtok not persisted: %+v", out)
	}
}

// TestModelCachedPriceMutuallyExclusive 同时设"免费"和"缓存价"是矛盾配置，
// 必须在写入时被拒（否则口径不明，账目迟早出错）。
func TestModelCachedPriceMutuallyExclusive(t *testing.T) {
	_, _, srv := setup(t)

	resp := do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o", map[string]any{
		"provider":              "openai",
		"input_per_mtok":        2.5,
		"cached_input_per_mtok": 1.25,
		"cached_input_free":     true,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("contradictory cached pricing expected 422, got %d", resp.StatusCode)
	}

	// 只设免费开关应当被接受
	resp2 := do(t, http.MethodPut, srv.URL+"/api/model/set/gpt-4o", map[string]any{
		"provider":          "openai",
		"input_per_mtok":    2.5,
		"cached_input_free": true,
	}, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("cached_input_free alone expected 200, got %d", resp2.StatusCode)
	}
}
