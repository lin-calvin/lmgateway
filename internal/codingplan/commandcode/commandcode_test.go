package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

func newUpstream(t *testing.T, events []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/fingerprint/record", "/alpha/lifecycle-events":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case "/provider/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "deepseek/deepseek-v4-flash"},
				map[string]any{"id": "claude-sonnet-4-6"},
			}})
		case "/alpha/generate":
			w.Header().Set("Content-Type", "application/x-ndjson")
			fl := w.(http.Flusher)
			for _, event := range events {
				b, _ := json.Marshal(event)
				fmt.Fprintf(w, "%s\n", b)
				fl.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func serveIdentity(pkt packet.Packet, _ ...string) packet.Packet { return pkt }

func TestCommandCodeChatStream(t *testing.T) {
	up := newUpstream(t, []map[string]any{
		{"type": "reasoning-delta", "text": "think "},
		{"type": "text-delta", "text": "hello "},
		{"type": "text-delta", "text": "world"},
		{"type": "finish-step", "finishReason": "stop", "usage": map[string]any{"inputTokens": 10, "outputTokens": 3}},
		{"type": "finish", "totalUsage": map[string]any{"inputTokens": 10, "outputTokens": 3, "cachedInputTokens": 4}},
	})
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test"})
	doc, err := lm.NewRequest("openai", map[string]any{
		"model": "deepseek/deepseek-v4-flash", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if out.Phase() != packet.PhaseStream {
		t.Fatalf("expected stream phase, got %q", out.Phase())
	}
	response, _ := out.Response()
	value, _ := response.Get("event")
	events, ok := value.(<-chan lm.LMEvent)
	if !ok {
		t.Fatal("missing event channel")
	}
	var content, reasoning string
	for event := range events {
		raw, _ := lm.MapEvent(event)
		choices, _ := raw["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		content += asString(delta["content"])
		reasoning += asString(delta["reasoning_content"])
	}
	if content != "hello world" || reasoning != "think " {
		t.Fatalf("unexpected stream content=%q reasoning=%q", content, reasoning)
	}
}

func TestCommandCodeChatNonStream(t *testing.T) {
	up := newUpstream(t, []map[string]any{
		{"type": "text-delta", "text": "hi there"},
		{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 5, "outputTokens": 2}},
	})
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test"})
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	response, _ := out.Response()
	content, _ := response.Get("choices")
	choices, _ := content.([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hi there" {
		t.Fatalf("unexpected non-stream content: %v", message)
	}
}

func TestCommandCodeResponsesClient(t *testing.T) {
	up := newUpstream(t, []map[string]any{
		{"type": "text-delta", "text": "OK"},
		{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 2, "outputTokens": 1}},
	})
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test"})
	doc, err := lm.NewRequest("openai_response", map[string]any{
		"model": "deepseek/deepseek-v4-flash", "stream": true,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	response, _ := out.Response()
	value, _ := response.Get("event")
	events, ok := value.(<-chan lm.LMEvent)
	if !ok {
		t.Fatal("missing event channel")
	}
	var text string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				goto done
			}
			if lm.EventName(event) == "response.output_text.delta" {
				raw, _ := lm.MapEvent(event)
				text += asString(raw["delta"])
			}
		case <-deadline:
			t.Fatal("timeout waiting for responses events")
		}
	}
done:
	if !strings.Contains(text, "OK") {
		t.Fatalf("expected responses text OK, got %q", text)
	}
}

func TestCommandCodeDiscover(t *testing.T) {
	up := newUpstream(t, nil)
	defer up.Close()
	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test"})
	models, err := o.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
}

func TestBuildCCRequestSystemAndTools(t *testing.T) {
	body := buildCCRequest(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "bash", "description": "run", "parameters": map[string]any{"type": "object"},
		}}},
		"reasoning_effort": "high",
	}, "upstream-m", "", Config{EmptySystemPlaceholder: true, CLIMode: "agent"})

	params, _ := body["params"].(map[string]any)
	if params["model"] != "upstream-m" || params["stream"] != true {
		t.Fatalf("unexpected params: %v", params)
	}
	system, _ := params["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "sys" {
		t.Fatalf("unexpected system blocks: %v", system)
	}
	tools, _ := params["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "bash" {
		t.Fatalf("unexpected tools: %v", tools)
	}
	if params["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort not forwarded: %v", params)
	}
}

func TestBuildCCRequestCacheMarker(t *testing.T) {
	body := buildCCRequest(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "a"}}},
			map[string]any{"role": "system", "content": "b"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, "", "cache-key", Config{CLIMode: "agent"})

	params, _ := body["params"].(map[string]any)
	system, _ := params["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("expected 2 system blocks, got %v", system)
	}
	if system[0].(map[string]any)["text"] != "a\n" {
		t.Fatalf("non-final block should gain newline: %v", system[0])
	}
	if system[1].(map[string]any)["cache_control"] == nil {
		t.Fatalf("prompt_cache_key should mark last system block: %v", system[1])
	}
}

func TestBuildCCRequestPreservesCacheMarker(t *testing.T) {
	marker := map[string]any{"type": "ephemeral"}
	body := buildCCRequest(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "a", "cache_control": marker}}},
			map[string]any{"role": "system", "content": "b"},
		},
	}, "", "cache-key", Config{CLIMode: "agent"})

	params, _ := body["params"].(map[string]any)
	system, _ := params["system"].([]any)
	if system[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("existing marker should be preserved: %v", system[0])
	}
	if system[1].(map[string]any)["cache_control"] != nil {
		t.Fatalf("existing marker should suppress new marker: %v", system[1])
	}
}

func TestApplyThreadID(t *testing.T) {
	body := map[string]any{
		"config": map[string]any{}, "memory": nil, "taste": nil, "skills": nil,
		"permissionMode": "standard", "mode": "agent", "params": map[string]any{},
	}
	applyThreadID(body, "12345678-1234-1234-1234-123456789abc")
	if body["threadId"] != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("threadId not applied: %v", body["threadId"])
	}
	body2 := map[string]any{"config": map[string]any{}}
	applyThreadID(body2, "not-a-uuid")
	if _, ok := body2["threadId"]; ok {
		t.Fatalf("invalid session id must not become threadId")
	}
}

func TestMapCCEventError(t *testing.T) {
	out := mapCCEventError(map[string]any{
		"type":  "error",
		"error": map[string]any{"message": "<429> slow down", "statusCode": 429, "code": "USAGE_EXCEEDED"},
	})
	if out["type"] != "rate_limit_error" || out["retry_after"] != 30 || out["code"] != "USAGE_EXCEEDED" {
		t.Fatalf("unexpected mapped error: %v", out)
	}
	if out["message"] != "<429> slow down" {
		t.Fatalf("message lost: %v", out)
	}
}

func TestOpenStreamRetriesBeforeFirstEvent(t *testing.T) {
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/fingerprint/record", "/alpha/lifecycle-events":
			w.WriteHeader(http.StatusOK)
		case "/alpha/generate":
			calls++
			w.Header().Set("Content-Type", "application/x-ndjson")
			if calls == 1 {
				return // drop the connection without emitting any event
			}
			fl := w.(http.Flusher)
			for _, event := range []map[string]any{
				{"type": "text-delta", "text": "ok"},
				{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 1, "outputTokens": 1}},
			} {
				b, _ := json.Marshal(event)
				fmt.Fprintf(w, "%s\n", b)
				fl.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test", RetryMax: 2, RetryBaseMS: 1})
	doc, err := lm.NewRequest("openai", map[string]any{
		"model": "deepseek/deepseek-v4-flash", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	response, _ := out.Response()
	value, _ := response.Get("event")
	events, _ := value.(<-chan lm.LMEvent)
	var content string
	for event := range events {
		raw, _ := lm.MapEvent(event)
		choices, _ := raw["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		content += asString(delta["content"])
	}
	if content != "ok" {
		t.Fatalf("expected retried content ok, got %q", content)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", calls)
	}
}

func TestOpenStreamIdleTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/fingerprint/record", "/alpha/lifecycle-events":
			w.WriteHeader(http.StatusOK)
		case "/alpha/generate":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test", StreamIdleMS: 50, RetryMax: 2, RetryBaseMS: 1})
	doc, err := lm.NewRequest("openai", map[string]any{
		"model": "deepseek/deepseek-v4-flash", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt, serveIdentity)
	if _, ok := out.Error(); !ok {
		t.Fatalf("expected idle timeout error")
	}
}

func TestCompleteRetriesIncompleteStream(t *testing.T) {
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/fingerprint/record", "/alpha/lifecycle-events":
			w.WriteHeader(http.StatusOK)
		case "/alpha/generate":
			calls++
			w.Header().Set("Content-Type", "application/x-ndjson")
			fl := w.(http.Flusher)
			events := []map[string]any{{"type": "text-delta", "text": "partial "}}
			if calls >= 2 {
				events = append(events,
					map[string]any{"type": "text-delta", "text": "complete"},
					map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 1, "outputTokens": 2}},
				)
			}
			for _, event := range events {
				b, _ := json.Marshal(event)
				fmt.Fprintf(w, "%s\n", b)
				fl.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	o := New(Config{Name: "commandcode", BaseURL: up.URL, APIKey: "user_test", RetryMax: 2, RetryBaseMS: 1})
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	response, _ := out.Response()
	content, _ := response.Get("choices")
	choices, _ := content.([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "partial complete" {
		t.Fatalf("unexpected content after retry: %v", message)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", calls)
	}
}
