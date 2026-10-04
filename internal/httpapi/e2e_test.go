package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/httpapi"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

// e2e: mock upstream + real gateway, walk through
// request -> dispatcher(req group) -> openai handler (call upstream, flip resp)
// -> dispatcher(resp group) -> usage log -> respond fallback -> write back.
func TestChatCompletionsE2E(t *testing.T) {
	// 1. mock upstream (OpenAI compatible)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream got unexpected path: %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("upstream got wrong Authorization: %q", auth)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("upstream decode request failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-mock-1",
			"object":  "chat.completion",
			"model":   body["model"],
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hello from mock"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer upstream.Close()

	// 2. programmatic config (equivalent to config.yaml): model 实体自动接线路由
	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{
			Name:    "openai",
			Type:    "openai",
			BaseURL: upstream.URL,
			APIKey:  "test-key",
		}},
		Models: []config.ModelCfg{{Name: "gpt-4o-mini", Provider: "openai"}},
	})
	if err != nil {
		t.Fatalf("config.Build failed: %v", err)
	}

	// 3. start gateway
	gw := httptest.NewServer(httpapi.New(httpapi.FromDispatcher(rt.Dispatcher)))
	defer gw.Close()

	// 4. send OpenAI-compatible request
	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode gateway response failed: %v", err)
	}
	if out["model"] != "gpt-4o-mini" {
		t.Errorf("model passthrough wrong: %v", out["model"])
	}
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "hello from mock") {
		t.Errorf("content passthrough wrong: %q", content)
	}
	if _, ok := out["usage"]; !ok {
		t.Error("response missing usage")
	}
}

// error path: upstream 500 -> openai handler turns error packet -> resp respond fallback -> gateway 502
func TestUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"upstream boom"}}`, http.StatusInternalServerError)
	}))
	defer upstream.Close()

	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{
			Name: "openai", Type: "openai", BaseURL: upstream.URL, APIKey: "k",
		}},
		Models: []config.ModelCfg{{Name: "m", Provider: "openai"}},
	})
	if err != nil {
		t.Fatalf("config.Build failed: %v", err)
	}

	gw := httptest.NewServer(httpapi.New(httpapi.FromDispatcher(rt.Dispatcher)))
	defer gw.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on upstream error, got %d", resp.StatusCode)
	}
}

// in-chain miss: request doesn't match any req rule -> 404 (no_route)
func TestNoRoute404(t *testing.T) {
	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{
			Name: "openai", Type: "openai", BaseURL: "http://127.0.0.1:1", APIKey: "k",
		}},
		Models: []config.ModelCfg{{Name: "gpt-4o", Provider: "openai"}},
	})
	if err != nil {
		t.Fatalf("config.Build failed: %v", err)
	}

	gw := httptest.NewServer(httpapi.New(httpapi.FromDispatcher(rt.Dispatcher)))
	defer gw.Close()

	// model "claude-3-5" does not match the gpt- prefix rule -> 404, upstream must NOT be hit
	reqBody, _ := json.Marshal(map[string]any{
		"model": "claude-3-5", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on no route, got %d", resp.StatusCode)
	}
}

// /v1/models：从 config Models[] 扣库存，热更自动反映
func TestModelsEndpoint(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 1000, Interval: time.Hour})
	defer ts.Close()

	for _, p := range []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}} {
		js.Put(ctx, config.ProviderKey(p.Name), p)
	}
	for _, m := range []config.ModelCfg{
		{Name: "agent", Provider: "openai"},
		{Name: "claude", Provider: "openai"},
	} {
		js.Put(ctx, config.ModelKey(m.Name), m)
	}

	mgr := config.NewManager(js, ts)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(httpapi.New(mgr))
	defer gw.Close()

	modelIDs := func() map[string]string {
		t.Helper()
		resp, err := http.Get(gw.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var out struct {
			Data []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
				Object  string `json:"object"`
			} `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		m := map[string]string{}
		for _, d := range out.Data {
			if d.Object != "model" {
				t.Errorf("unexpected object %q", d.Object)
			}
			m[d.ID] = d.OwnedBy
		}
		return m
	}

	ids := modelIDs()
	if ids["agent"] != "openai" || ids["claude"] != "openai" {
		t.Errorf("models wrong: %v", ids)
	}

	// 热更：新增 model → /v1/models 反映
	js.Put(ctx, config.ModelKey("gemini"), config.ModelCfg{Name: "gemini", Provider: "openai"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ids = modelIDs(); len(ids) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ids["gemini"] != "openai" {
		t.Errorf("hot-added model not reflected: %v", ids)
	}
}

// config validation: rule missing from -> Build error
func TestRuleRequiresFromAction(t *testing.T) {
	_, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Rules: []config.RuleCfg{
			{Action: "openai"}, // missing from
		},
	})
	if err == nil || !strings.Contains(err.Error(), "from is required") {
		t.Fatalf("expected missing from error, got: %v", err)
	}

	// 纯变换规则：无 action 无 to → 报错
	_, err = config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x"}},
		Rules: []config.RuleCfg{
			{From: "http", Set: map[string]any{"model": "x"}}, // 无 action 无 to
		},
	})
	if err == nil || !strings.Contains(err.Error(), "action or to is required") {
		t.Fatalf("expected action/to error, got: %v", err)
	}
}

// 自动接线：providers+models 无需任何规则即可工作（model 路由 + provider→usage + 全局 respond）
func TestAutoWiringNoRules(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "model": r.URL.Query().Get("m"),
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: upstream.URL, APIKey: "k"}},
		Models:    []config.ModelCfg{{Name: "gpt", Provider: "openai", Default: true}},
	})
	if err != nil {
		t.Fatalf("Build with no rules should succeed: %v", err)
	}
	// 规则表应包含：http 路由边 + provider→usage 自动边 + 全局 respond
	rules := rt.Table.All()
	if len(rules) < 3 {
		t.Fatalf("expected >=3 auto rules, got %d", len(rules))
	}
}

func streamChunkJSON(id, delta string) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": "gpt-4o-mini",
		"choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": delta}, "finish_reason": nil}},
	})
	return string(b)
}

// e2e streaming: mock 上游分块 SSE + usage + [DONE] → 网关 SSE 帧转发到客户端，
// 内容拼接正确、以 data: [DONE] 结尾。
func TestCORSPreflight(t *testing.T) {
	t.Setenv("LMGATEWAY_CORS_ORIGINS", "")
	h := httpapi.WithCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("unexpected allow origin: %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("authorization header not allowed: %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestCORSAllowlist(t *testing.T) {
	t.Setenv("LMGATEWAY_CORS_ORIGINS", "https://allowed.example, https://other.example")
	h := httpapi.WithCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	allowed := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	allowed.Header.Set("Origin", "https://allowed.example")
	allowedRec := httptest.NewRecorder()
	h.ServeHTTP(allowedRec, allowed)
	if allowedRec.Header().Get("Access-Control-Allow-Origin") != "https://allowed.example" {
		t.Fatalf("allowlisted origin not reflected: %q", allowedRec.Header().Get("Access-Control-Allow-Origin"))
	}
	blocked := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	blocked.Header.Set("Origin", "https://blocked.example")
	blockedRec := httptest.NewRecorder()
	h.ServeHTTP(blockedRec, blocked)
	if blockedRec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("blocked origin should not receive CORS header: %q", blockedRec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestStreamSpendRecordsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", streamChunkJSON("c1", "hello"))
		fl.Flush()
		u, _ := json.Marshal(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ""}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
		fmt.Fprintf(w, "data: %s\n\n", u)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 1000, Interval: time.Hour})
	defer ts.Close()
	m := config.NewManagerWithBase(js, ts, config.Config{
		Providers: []config.ProviderCfg{{Name: "deepseek", Type: "openai", BaseURL: upstream.URL, APIKey: "k"}},
		Models:    []config.ModelCfg{{Name: "m", Provider: "deepseek"}},
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(httpapi.New(m))
	defer gw.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, data)
	}
	flush, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ts.Flush(flush); err != nil {
		t.Fatal(err)
	}
	recs, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 spend record, got %d", len(recs))
	}
	fields := recs[0].Fields
	if fields["prompt_tokens"] != 10.0 || fields["completion_tokens"] != 20.0 || fields["total_tokens"] != 30.0 {
		t.Fatalf("spend usage not recorded: %v", fields)
	}
}

func TestChatCompletionsStreamE2E(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, s := range []string{"你好", "，", "世界"} {
			fmt.Fprintf(w, "data: %s\n\n", streamChunkJSON("c1", s))
			fl.Flush()
		}
		u, _ := json.Marshal(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": "gpt-4o-mini",
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
		fmt.Fprintf(w, "data: %s\n\n", u)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{
			Name: "openai", Type: "openai", BaseURL: upstream.URL, APIKey: "k",
		}},
		Models: []config.ModelCfg{{Name: "gpt-4o-mini", Provider: "openai"}},
	})
	if err != nil {
		t.Fatalf("config.Build failed: %v", err)
	}

	gw := httptest.NewServer(httpapi.New(httpapi.FromDispatcher(rt.Dispatcher)))
	defer gw.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected SSE content-type, got %q", ct)
	}

	// 逐行读 SSE
	sc := bufio.NewScanner(resp.Body)
	var data []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream failed: %v", err)
	}

	// 内容块 + usage 块 + [DONE]
	if len(data) < 5 {
		t.Fatalf("expected >=5 data events, got %d: %v", len(data), data)
	}
	var content string
	gotDone := false
	for _, d := range data {
		if d == "[DONE]" {
			gotDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(d), &chunk); err != nil {
			t.Fatalf("bad chunk json %q: %v", d, err)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			delta := choices[0].(map[string]any)["delta"].(map[string]any)
			if c, ok := delta["content"].(string); ok {
				content += c
			}
		}
	}
	if content != "你好，世界" {
		t.Errorf("streamed content mismatch: %q", content)
	}
	if !gotDone {
		t.Error("missing data: [DONE] terminal event")
	}
	if data[len(data)-1] != "[DONE]" {
		t.Errorf("[DONE] should be last event, got %q", data[len(data)-1])
	}
}

func TestLogprobsWebhookE2E(t *testing.T) {
	webhook := make(chan map[string]any, 1)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		select {
		case webhook <- payload:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upstreamBody)
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": nil,
				"logprobs": map[string]any{"content": []any{map[string]any{"token": "hi", "logprob": -0.2}}},
			}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	rt, err := config.Build(config.Config{
		Providers: []config.ProviderCfg{{Name: "deepseek", Type: "openai", BaseURL: upstream.URL, APIKey: "k"}},
		Models:    []config.ModelCfg{{Name: "m", Provider: "deepseek"}},
		Logprobs:  config.LogprobsCfg{WebhookURL: webhookSrv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(httpapi.New(httpapi.FromDispatcher(rt.Dispatcher)))
	defer gw.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
		"logprobs": true, "top_logprobs": 5,
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if upstreamBody["logprobs"] != true || upstreamBody["top_logprobs"] != float64(5) {
		t.Fatalf("gateway must preserve client logprobs request: %v", upstreamBody)
	}
	select {
	case payload := <-webhook:
		logprobs, _ := payload["logprobs"].(map[string]any)
		if logprobs["format"] != "chat" {
			t.Fatalf("unexpected webhook payload: %v", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook not called")
	}
}
