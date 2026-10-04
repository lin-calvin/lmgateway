package sse

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestStreamNext(t *testing.T) {
	sample := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"event: ping\n" +
		": comment\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	s := New(io.NopCloser(strings.NewReader(sample)))

	c1, err := s.Next()
	if err != nil {
		t.Fatalf("Next 1 failed: %v", err)
	}
	if got := c1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"]; got != "你" {
		t.Errorf("chunk1 content mismatch: %v", got)
	}

	c2, err := s.Next()
	if err != nil {
		t.Fatalf("Next 2 failed: %v", err)
	}
	if got := c2["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"]; got != "好" {
		t.Errorf("chunk2 content mismatch: %v", got)
	}

	// event:/: 注释被忽略，usage 块正常返回
	if _, err := s.Next(); err != nil {
		t.Fatalf("Next 3 failed: %v", err)
	}
	if s.Usage == nil || s.Usage["total_tokens"] != float64(5) {
		t.Errorf("usage not collected: %v", s.Usage)
	}

	// [DONE] → EOF
	if _, err := s.Next(); err != io.EOF {
		t.Errorf("after [DONE] expected EOF, got %v", err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Errorf("after done expected EOF, got %v", err)
	}
}

// SSE 规范：同一事件内连续多个 data: 行用 \n 拼接
func TestStreamMultilineData(t *testing.T) {
	s := New(io.NopCloser(strings.NewReader("data: {\"id\":\"c1\",\ndata: \"choices\":[]}\n\n")))
	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if _, ok := chunk["id"]; !ok {
		t.Errorf("multiline join missing id: %v", chunk)
	}
	if _, ok := chunk["choices"]; !ok {
		t.Errorf("multiline join missing choices: %v", chunk)
	}
}

func TestStreamEmpty(t *testing.T) {
	s := New(io.NopCloser(strings.NewReader("")))
	if _, err := s.Next(); err != io.EOF {
		t.Errorf("empty stream expected EOF, got %v", err)
	}
}

func TestStreamBadJSON(t *testing.T) {
	s := New(io.NopCloser(strings.NewReader("data: {not json}\n\n")))
	if _, err := s.Next(); err == nil {
		t.Error("bad json should error")
	}
}

func TestResponsesToolCallDeltasKeepFunctionName(t *testing.T) {
	sample := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item1\",\"call_id\":\"call1\",\"name\":\"shell\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item1\",\"delta\":\"{\\\"cmd\\\":\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item1\",\"delta\":\"pwd\\\"}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-5.6-luna\"}}\n\n"
	s := NewResponses(io.NopCloser(strings.NewReader(sample)))
	for i := 0; i < 3; i++ {
		chunk, err := s.Next()
		if err != nil {
			t.Fatalf("tool chunk %d failed: %v", i, err)
		}
		call := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		fn := call["function"].(map[string]any)
		if fn["name"] != "shell" {
			t.Fatalf("tool chunk %d missing function name: %v", i, chunk)
		}
	}
	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("completion chunk failed: %v", err)
	}
	if chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("expected tool_calls finish reason: %v", chunk)
	}
}

func TestResponsesReasoningSummaryDelta(t *testing.T) {
	s := NewResponses(io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.reasoning_summary.delta\",\"delta\":\"thinking\"}\n\n",
	)))
	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("reasoning summary chunk failed: %v", err)
	}
	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["reasoning_content"] != "thinking" {
		t.Fatalf("reasoning summary was not projected to reasoning_content: %v", chunk)
	}
}

func TestNativeResponsesEventsPreserveNamesAndData(t *testing.T) {
	s := NewNativeResponses(io.NopCloser(strings.NewReader(
		"event: response.reasoning_summary_text.delta\n" +
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs1\",\"summary_index\":0,\"delta\":\"First\\nSecond\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n",
	)))
	event, err := s.NextEvent()
	if err != nil {
		t.Fatalf("native event failed: %v", err)
	}
	if event.Name != "response.reasoning_summary_text.delta" || !strings.Contains(string(event.Data), "First\\nSecond") {
		t.Fatalf("native event was changed: %+v", event)
	}
	event, err = s.NextEvent()
	if err != nil {
		t.Fatalf("native completion failed: %v", err)
	}
	if event.Name != "response.completed" {
		t.Fatalf("unexpected completion event: %+v", event)
	}
	if _, err := s.NextEvent(); err != io.EOF {
		t.Fatalf("native stream should end after completion, got %v", err)
	}
}

func TestNativeResponsesBuildsResponseFromDeltaEvents(t *testing.T) {
	s := NewNativeResponses(io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m1\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
			"data: {\"type\":\"response.content_part.added\",\"item_id\":\"m1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\"}}\n\n",
	)))
	for {
		if _, err := s.NextEvent(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if s.Response == nil || s.Response["id"] != "r1" {
		t.Fatalf("native response snapshot missing: %v", s.Response)
	}
	output := s.Response["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("native delta was not accumulated: %v", s.Response)
	}
}

func TestChatAsResponsesPreservesTextAndToolCalls(t *testing.T) {
	chunk := func(value map[string]any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(data) + "\n\n"
	}
	sample := chunk(map[string]any{
		"id": "c1", "model": "upstream",
		"choices": []any{map[string]any{"delta": map[string]any{
			"content":    "hello",
			"tool_calls": []any{map[string]any{"index": 0, "id": "call1", "type": "function", "function": map[string]any{"name": "shell", "arguments": "{\"cmd\":\""}}},
		}}},
	})
	sample += chunk(map[string]any{
		"id": "c1",
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "pwd\"}"}}},
		}}},
	})
	sample += chunk(map[string]any{"id": "c1", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
	sample += "data: [DONE]\n\n"
	s := NewChatAsResponses(io.NopCloser(strings.NewReader(sample)))
	s.SetResponseModel("client-model")
	var names []string
	for {
		event, err := s.NextEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, event.Name)
	}
	completed := 0
	for _, name := range names {
		if name == "response.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("expected one completion event, got %d: %v", completed, names)
	}
	if s.Response == nil || s.Response["model"] != "client-model" {
		t.Fatalf("client model was not preserved: %v", s.Response)
	}
	output := s.Response["output"].([]any)
	messageText := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if messageText != "hello" {
		t.Fatalf("chat text was not converted: %v", s.Response)
	}
	tool := output[1].(map[string]any)
	if tool["name"] != "shell" || tool["arguments"] != "{\"cmd\":\"pwd\"}" {
		t.Fatalf("chat tool call was not converted: %v", tool)
	}
	if s.ResponseUsage["input_tokens"] != float64(2) {
		t.Fatalf("chat usage was not converted: %v", s.ResponseUsage)
	}
}
