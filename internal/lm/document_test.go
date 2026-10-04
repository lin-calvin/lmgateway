package lm

import (
	"context"
	"reflect"
	"testing"
)

func TestNewRequestOmitsNullOptionalFields(t *testing.T) {
	doc, err := NewRequest("openai", map[string]any{
		"model":             "m",
		"max_output_tokens": nil,
		"stream_options":    nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Map(doc)
	if _, ok := raw["max_output_tokens"]; ok {
		t.Error("null max_output_tokens should be treated as absent")
	}
	if _, ok := raw["stream_options"]; ok {
		t.Error("null stream_options should be treated as absent")
	}
}

func TestRegistryRTTIAndRawMap(t *testing.T) {
	raw := map[string]any{"model": "agent", "messages": []any{}}
	doc, err := NewRequest("openai", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := TypeOf(doc); !ok || got != "openai" {
		t.Fatalf("unexpected document type: %q %v", got, ok)
	}
	if !IsRequest(doc) || IsResponse(doc) {
		t.Fatalf("unexpected request/response classification")
	}
	mapped, ok := Map(doc)
	if !ok || mapped["model"] != "agent" {
		t.Fatalf("raw map was not preserved: %v", mapped)
	}
	if err := doc.Set("model", "gpt-5.6-luna"); err != nil {
		t.Fatal(err)
	}
	if raw["model"] != "gpt-5.6-luna" {
		t.Fatalf("setter did not mutate source map: %v", raw)
	}
}

func TestProtocolSemanticSet(t *testing.T) {
	chat, err := NewRequest("openai", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.Set("reasoning.effort", "max"); err != nil {
		t.Fatal(err)
	}
	chatRaw, _ := Map(chat)
	if chatRaw["reasoning_effort"] != "max" {
		t.Fatalf("chat reasoning setter wrong: %v", chatRaw)
	}

	responses, err := NewRequest("openai_response", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := responses.Set("reasoning.effort", "max"); err != nil {
		t.Fatal(err)
	}
	responsesRaw, _ := Map(responses)
	reasoning, ok := responsesRaw["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "max" {
		t.Fatalf("responses reasoning setter wrong: %v", responsesRaw)
	}
}

func TestProtocolSemanticSetDefault(t *testing.T) {
	explicit, err := NewRequest("openai", map[string]any{"reasoning_effort": "low"})
	if err != nil {
		t.Fatal(err)
	}
	if err := explicit.SetDefault("reasoning.effort", "max"); err != nil {
		t.Fatal(err)
	}
	raw, _ := Map(explicit)
	if raw["reasoning_effort"] != "low" {
		t.Fatalf("client value should win over default: %v", raw)
	}

	absent, err := NewRequest("openai", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := absent.SetDefault("reasoning.effort", "max"); err != nil {
		t.Fatal(err)
	}
	raw, _ = Map(absent)
	if raw["reasoning_effort"] != "max" {
		t.Fatalf("default should fill absent field: %v", raw)
	}

	empty, err := NewRequest("openai_response", map[string]any{"reasoning": map[string]any{"effort": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.SetDefault("reasoning.effort", "high"); err != nil {
		t.Fatal(err)
	}
	raw, _ = Map(empty)
	reasoning, _ := raw["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("default should fill empty string: %v", raw)
	}
}

func TestProtocolConvertFrom(t *testing.T) {
	chat, err := NewRequest("openai", map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "rules"},
			map[string]any{"role": "user", "content": "hello"},
		},
		"max_tokens":       42,
		"reasoning_effort": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	responses, err := NewRequest("openai_response", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	converted, err := responses.ConvertFrom(chat)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := TypeOf(converted); !ok || name != "openai_response" {
		t.Fatalf("converted document type wrong: %q %v", name, ok)
	}
	if reflect.ValueOf(converted).Pointer() == reflect.ValueOf(chat).Pointer() {
		t.Fatal("cross-protocol conversion unexpectedly returned source")
	}
	raw, _ := Map(converted)
	if raw["model"] != "m" || raw["instructions"] != "rules" || raw["max_output_tokens"] != 42 {
		t.Fatalf("converted response request wrong: %v", raw)
	}
	reasoning, ok := raw["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" {
		t.Fatalf("converted reasoning wrong: %v", raw)
	}

	noop, err := responses.ConvertFrom(converted)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(noop).Pointer() != reflect.ValueOf(converted).Pointer() {
		t.Fatal("same-protocol conversion did not noop")
	}
}

func TestRequestConversionPreservesConstraintsAndMultimodalInput(t *testing.T) {
	chat, err := NewRequest("openai", map[string]any{
		"model": "chat-model",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,x"}},
			},
		}},
		"max_completion_tokens": 17,
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "answer", "schema": map[string]any{"type": "object"}, "strict": true},
		},
		"vendor_extension": map[string]any{"enabled": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	responses, err := NewRequest("openai_response", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	converted, err := responses.ConvertFrom(chat)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Map(converted)
	if raw["max_output_tokens"] != 17 || raw["vendor_extension"].(map[string]any)["enabled"] != true {
		t.Fatalf("request constraints or extension lost: %v", raw)
	}
	text := raw["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" {
		t.Fatalf("response format was not converted: %v", text)
	}
	input := raw["input"].([]any)
	content := input[0].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image content was not converted: %v", content)
	}

	back, err := NewRequest("openai", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	back, err = back.ConvertFrom(converted)
	if err != nil {
		t.Fatal(err)
	}
	backRaw, _ := Map(back)
	messages := backRaw["messages"].([]any)
	backContent := messages[0].(map[string]any)["content"].([]any)
	if backContent[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image content was not restored: %v", backContent)
	}
	if backRaw["max_tokens"] != 17 {
		t.Fatalf("max output tokens was not restored: %v", backRaw)
	}
}

func TestResponsesStringInputAndFinishStatusConversion(t *testing.T) {
	responses, err := NewRequest("openai_response", map[string]any{
		"model": "response-model",
		"input": "hello",
		"text":  map[string]any{"format": map[string]any{"type": "json_object"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := NewRequest("openai", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	converted, err := chat.ConvertFrom(responses)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Map(converted)
	messages := raw["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("string Responses input was lost: %v", raw)
	}
	if raw["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("Responses text format was not converted: %v", raw)
	}

	response, err := NewResponse("openai_response", map[string]any{
		"id": "r1", "model": "response-model", "status": "incomplete",
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
		"output":             []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	chatResponse, err := NewResponse("openai", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	convertedResponse, err := chatResponse.ConvertFrom(response)
	if err != nil {
		t.Fatal(err)
	}
	chatRaw, _ := Map(convertedResponse)
	choice := chatRaw["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("incomplete response finish reason was lost: %v", chatRaw)
	}
}

func TestCloneDoesNotMutateSource(t *testing.T) {
	source, err := NewRequest("openai", map[string]any{"model": "client", "messages": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := Clone(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloned.Set("model", "upstream"); err != nil {
		t.Fatal(err)
	}
	if model, _ := source.Get("model"); model != "client" {
		t.Fatalf("clone mutation changed source: %v", model)
	}
}

func TestResponseStreamEventPathAndFinalDocument(t *testing.T) {
	input := make(chan LMEvent, 2)
	stream, err := NewResponseStream("openai", input, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stream.Get("event"); !ok {
		t.Fatal("stream response should expose event channel")
	}
	if _, ok := stream.Get("context"); !ok {
		t.Fatal("stream response should expose context")
	}
	input <- OpenAIChatEvent(map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}},
	})
	input <- OpenAIChatEvent(map[string]any{
		"id": "c1", "model": "m", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	})
	close(input)
	events, _ := stream.Get("event")
	for range events.(<-chan LMEvent) {
	}
	if model, _ := stream.Get("model"); model != "m" {
		t.Fatalf("final model was not accumulated: %v", model)
	}
	usage, _ := stream.Get("usage")
	if usage.(map[string]any)["total_tokens"] != 3 {
		t.Fatalf("final usage was not accumulated: %v", usage)
	}
}

func TestResponseStreamConversionIsLazyAndStateful(t *testing.T) {
	input := make(chan LMEvent, 1)
	source, err := NewResponseStream("openai", input, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewResponseTarget("openai_response", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	converted, err := target.ConvertFrom(source)
	if err != nil {
		t.Fatal(err)
	}
	input <- OpenAIChatEvent(map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}},
	})
	close(input)
	value, ok := converted.Get("event")
	if !ok {
		t.Fatal("converted response should expose event channel")
	}
	var names []string
	for event := range value.(<-chan LMEvent) {
		names = append(names, EventName(event))
	}
	if len(names) < 2 || names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("unexpected converted event sequence: %v", names)
	}
	if model, _ := converted.Get("model"); model != "m" {
		t.Fatalf("converted final model was not accumulated: %v", model)
	}
}

func TestResponseStreamResponsesToChatAccumulatesText(t *testing.T) {
	input := make(chan LMEvent, 3)
	source, err := NewResponseStream("openai_response", input, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewResponseTarget("openai", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	converted, err := target.ConvertFrom(source)
	if err != nil {
		t.Fatal(err)
	}
	input <- OpenAIResponsesEvent{"type": "response.created", "response": map[string]any{"id": "r1", "model": "m"}}
	input <- OpenAIResponsesEvent{"type": "response.output_text.delta", "delta": "OK"}
	input <- OpenAIResponsesEvent{"type": "response.completed", "response": map[string]any{"id": "r1", "model": "m", "status": "completed", "max_output_tokens": nil}}
	close(input)
	value, ok := converted.Get("event")
	if !ok {
		t.Fatal("converted response should expose event channel")
	}
	for range value.(<-chan LMEvent) {
	}
	raw, ok := Map(converted)
	if !ok {
		t.Fatal("converted response should expose raw document")
	}
	choices, _ := raw["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "OK" {
		t.Fatalf("converted Chat content was lost: %v", raw)
	}
}
