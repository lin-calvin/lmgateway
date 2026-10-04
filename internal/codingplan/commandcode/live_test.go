//go:build live

package commandcode

import (
	"context"
	"os"
	"testing"
	"time"

	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

func liveConfig(t *testing.T) Config {
	t.Helper()
	key := os.Getenv("CC_API_KEY")
	if key == "" {
		t.Skip("CC_API_KEY not set")
	}
	return Config{Name: "commandcode", APIKey: key, TimeoutSec: 300, EmptySystemPlaceholder: true}
}

func liveModel() string {
	if m := os.Getenv("CC_MODEL"); m != "" {
		return m
	}
	return "deepseek/deepseek-v4-flash"
}

func TestLiveDiscover(t *testing.T) {
	o := New(liveConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, err := o.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models discovered")
	}
	t.Logf("discovered %d models, first=%s", len(models), models[0].ID)
}

func TestLiveChat(t *testing.T) {
	o := New(liveConfig(t))
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":      liveModel(),
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pkt.Set(packet.KeyCtx, ctx)

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("chat error: %v", msg)
	}
	resp, _ := out.Response()
	raw, _ := lm.Map(resp)
	choices, _ := raw["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices: %v", raw)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	content := asString(msg["content"])
	if content == "" {
		t.Fatalf("empty content: %v", msg)
	}
	t.Logf("chat ok: content=%q usage=%v finish=%v", content, raw["usage"], choices[0].(map[string]any)["finish_reason"])
}

func TestLiveStream(t *testing.T) {
	o := New(liveConfig(t))
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":      liveModel(),
		"stream":     true,
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "Count from 1 to 5, one number per line."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pkt.Set(packet.KeyCtx, ctx)

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("stream error: %v", msg)
	}
	resp, _ := out.Response()
	value, _ := resp.Get("event")
	events, _ := value.(<-chan lm.LMEvent)
	var content string
	for event := range events {
		raw, _ := lm.MapEvent(event)
		if errObj, ok := raw["error"]; ok {
			t.Fatalf("stream error event: %v", errObj)
		}
		choices, _ := raw["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		content += asString(delta["content"])
	}
	if content == "" {
		t.Fatal("empty streamed content")
	}
	t.Logf("stream ok: content=%q", content)
}

func TestLiveResponses(t *testing.T) {
	o := New(liveConfig(t))
	doc, err := lm.NewRequest("openai_response", map[string]any{
		"model":      liveModel(),
		"stream":     true,
		"max_tokens": 64,
		"input":      []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly: pong"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pkt.Set(packet.KeyCtx, ctx)

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("responses error: %v", msg)
	}
	resp, _ := out.Response()
	value, _ := resp.Get("event")
	events, _ := value.(<-chan lm.LMEvent)
	var text string
	for event := range events {
		if lm.EventName(event) == "response.output_text.delta" {
			raw, _ := lm.MapEvent(event)
			text += asString(raw["delta"])
		}
	}
	if text == "" {
		t.Fatal("empty responses text")
	}
	t.Logf("responses ok: text=%q", text)
}

func TestLiveToolCall(t *testing.T) {
	o := New(liveConfig(t))
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":      liveModel(),
		"stream":     true,
		"max_tokens": 128,
		"messages":   []any{map[string]any{"role": "user", "content": "What is the weather in Paris? You must call the get_weather tool."}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "get_weather",
			"description": "Get the current weather for a city",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"city": map[string]any{"type": "string"},
			}, "required": []any{"city"}},
		}}},
		"tool_choice": "required",
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pkt.Set(packet.KeyCtx, ctx)

	out := o.Handle(pkt, serveIdentity)
	if msg, ok := out.Error(); ok {
		t.Fatalf("tool error: %v", msg)
	}
	resp, _ := out.Response()
	value, _ := resp.Get("event")
	events, _ := value.(<-chan lm.LMEvent)
	var name, args string
	for event := range events {
		raw, _ := lm.MapEvent(event)
		choices, _ := raw["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		for _, rawCall := range asSlice(delta["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			name += asString(fn["name"])
			args += asString(fn["arguments"])
		}
	}
	if name != "get_weather" {
		t.Fatalf("expected get_weather tool call, got name=%q args=%q", name, args)
	}
	t.Logf("tool ok: name=%q args=%q", name, args)
}
