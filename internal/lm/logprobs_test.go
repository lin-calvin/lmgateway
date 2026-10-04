package lm

import (
	"context"
	"testing"
)

func TestChatStreamAccumulatesReturnedLogprobs(t *testing.T) {
	input := make(chan LMEvent, 2)
	stream, err := NewResponseStream("openai", input, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	input <- OpenAIChatEvent(map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "你"},
			"logprobs": map[string]any{"content": []any{map[string]any{"token": "你", "logprob": -0.1}}},
		}},
	})
	input <- OpenAIChatEvent(map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "好"},
			"logprobs": map[string]any{"content": []any{map[string]any{"token": "好", "logprob": -0.2}}},
		}},
	})
	close(input)

	events, _ := stream.Get("event")
	for range events.(<-chan LMEvent) {
	}
	raw, ok := Map(stream)
	if !ok {
		t.Fatal("stream should expose document")
	}
	choice, _ := asSlice(raw["choices"])[0].(map[string]any)
	logprobs, _ := choice["logprobs"].(map[string]any)
	entries := asSlice(logprobs["content"])
	if len(entries) != 2 {
		t.Fatalf("expected 2 returned logprob entries, got %v", logprobs)
	}
}
