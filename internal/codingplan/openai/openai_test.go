package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

func chunkJSON(id, delta string) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": "gpt-4o-mini",
		"choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": delta}, "finish_reason": nil}},
	})
	return string(b)
}

func streamingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, s := range []string{"你", "好", "！"} {
			fmt.Fprintf(w, "data: %s\n\n", chunkJSON("c1", s))
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
}

func TestOpenAIStream(t *testing.T) {
	up := streamingUpstream(t)
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "openai", BaseURL: up.URL, APIKey: "k"})
	pkt := packet.NewReq(map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt)
	if msg, isErr := out.Error(); isErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	if out.Phase() != packet.PhaseStream {
		t.Fatalf("expected phase=stream, got %q", out.Phase())
	}
	response, ok := out.Response()
	if !ok {
		t.Fatal("expected response document")
	}
	value, ok := response.Get("event")
	if !ok {
		t.Fatal("expected response event channel")
	}
	events := value.(<-chan lm.LMEvent)
	var content string
	for event := range events {
		data, err := lm.EventData(event)
		if err != nil {
			t.Fatal(err)
		}
		var chunk map[string]any
		if err := json.Unmarshal(data, &chunk); err != nil {
			t.Fatal(err)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			delta := choices[0].(map[string]any)["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				content += text
			}
		}
	}
	if content != "你好！" {
		t.Errorf("streamed content mismatch: %q", content)
	}
	usage, _ := response.Get("usage")
	if usage.(map[string]any)["total_tokens"] != float64(30) {
		t.Errorf("usage not collected: %v", usage)
	}
}

func TestOpenAIResponsesStream(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.reasoning_summary_text.delta\n")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n")
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "openai", BaseURL: up.URL, APIKey: "k", DocumentType: "openai_response"})
	doc, err := lm.NewRequest("openai_response", map[string]any{"model": "gpt-4o", "input": []any{}, "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if gotPath != "/responses" {
		t.Fatalf("expected responses endpoint, got %q", gotPath)
	}
	response, ok := out.Response()
	if !ok {
		t.Fatal("expected response document")
	}
	value, ok := response.Get("event")
	if !ok {
		t.Fatal("expected response event channel")
	}
	events := value.(<-chan lm.LMEvent)
	event, ok := <-events
	if !ok || lm.EventName(event) != "response.reasoning_summary_text.delta" {
		t.Fatalf("unexpected native event: %T %v", event, ok)
	}
}

func TestOpenAIStreamInjectsIncludeUsage(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", chunkJSON("c1", "hi"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "deepseek", BaseURL: up.URL, APIKey: "k"})
	pkt := packet.NewReq(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
	})
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	options, ok := gotBody["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("gateway must inject stream_options.include_usage: %v", gotBody)
	}
}

func TestOpenAIStreamUsageCanBeDisabled(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", chunkJSON("c1", "hi"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "x", BaseURL: up.URL, APIKey: "k", DisableStreamUsage: true})
	pkt := packet.NewReq(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
	})
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Fatalf("stream_options should be omitted when disabled: %v", gotBody)
	}
}

func TestOpenAIStreamUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "openai", BaseURL: up.URL, APIKey: "k"})
	pkt := packet.NewReq(map[string]any{
		"model": "m", "messages": []any{}, "stream": true,
	})
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt)
	if out.Phase() != packet.PhaseResp {
		t.Fatalf("expected resp-phase error packet, got %q", out.Phase())
	}
	if kind, _ := out.ErrorKind(); kind != packet.ErrUpstream {
		t.Errorf("expected upstream error kind, got %q", kind)
	}
}

// model 带 [provider]/ 前缀时剥离后再发上游（自动发现模型）
func TestOpenAIDiscoveredModelPrefix(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "c", "model": gotBody["model"], "choices": []any{}})
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "deepseek", BaseURL: up.URL, APIKey: "k"})
	pkt := packet.NewReq(map[string]any{
		"model":    "deepseek/deepseek-chat",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	pkt.Set(packet.KeyCtx, context.Background())
	out := o.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if gotBody["model"] != "deepseek-chat" {
		t.Errorf("prefix should be stripped upstream, got %v", gotBody["model"])
	}
}

// model extra_body 合并进上游请求体
func TestOpenAIExtraBody(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "c", "choices": []any{}})
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{
		Name: "x", BaseURL: up.URL, APIKey: "k",
		ExtraBody: map[string]map[string]any{
			"m": {"thinking": map[string]any{"type": "enabled"}},
		},
	})
	pkt := packet.NewReq(map[string]any{"model": "m", "messages": []any{}})
	pkt.Set(packet.KeyCtx, context.Background())

	out := o.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" {
		t.Errorf("extra_body not merged into upstream: %v", got)
	}
}

// 客户端断连 → ctx 取消 → 上游请求失败 → 错误包（不泄漏、不阻塞）
func TestOpenAIStreamCtxCancel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", chunkJSON("c1", "一"))
		fl.Flush()
		<-r.Context().Done() // 等取消
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{Name: "openai", BaseURL: up.URL, APIKey: "k"})
	ctx, cancel := context.WithCancel(context.Background())
	pkt := packet.NewReq(map[string]any{"model": "m", "messages": []any{}, "stream": true})
	pkt.Set(packet.KeyCtx, ctx)

	out := o.Handle(pkt)
	if out.Phase() != packet.PhaseStream {
		t.Fatalf("expected stream first, got %q", out.Phase())
	}
	response, ok := out.Response()
	if !ok {
		t.Fatal("expected response document")
	}
	value, ok := response.Get("event")
	if !ok {
		t.Fatal("expected response event channel")
	}
	events := value.(<-chan lm.LMEvent)
	if _, ok := <-events; !ok {
		t.Fatal("expected first event")
	}

	cancel()
	done := make(chan bool, 1)
	go func() {
		_, open := <-events
		done <- open
	}()
	select {
	case open := <-done:
		if open {
			t.Error("cancel: event channel remained open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not interrupt upstream read (leak)")
	}
}

func TestOpenAIModelDefaultExtraBody(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer up.Close()

	o := NewOpenAI(OpenAIConfig{
		Name: "openai", BaseURL: up.URL, APIKey: "k",
		DefaultExtraBody: map[string]map[string]any{"m": {"reasoning_effort": "max"}},
	})

	explicit := packet.NewReq(map[string]any{"model": "m", "messages": []any{}, "reasoning_effort": "low"})
	explicit.Set(packet.KeyCtx, context.Background())
	if msg, ok := o.Handle(explicit).Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("client value should win over model default: %v", got)
	}

	absent := packet.NewReq(map[string]any{"model": "m", "messages": []any{}})
	absent.Set(packet.KeyCtx, context.Background())
	if msg, ok := o.Handle(absent).Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if got["reasoning_effort"] != "max" {
		t.Fatalf("model default should fill absent field: %v", got)
	}
}
