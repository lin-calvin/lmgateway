package lm

import "testing"

func TestResponsesInputMergesReasoningWithToolCall(t *testing.T) {
	messages, err := responsesInputToMessages([]any{
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "think first"},
		}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	assistant, _ := messages[0].(map[string]any)
	if assistant["role"] != "assistant" || assistant["reasoning_content"] != "think first" || assistant["tool_calls"] == nil {
		t.Fatalf("assistant = %+v", assistant)
	}
	tool, _ := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool = %+v", tool)
	}
}

func TestMessagesToResponsesPreservesReasoning(t *testing.T) {
	_, input := messagesToResponsesInput([]any{
		map[string]any{
			"role": "assistant", "content": nil, "reasoning_content": "think first",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`},
			}},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	})
	if len(input) < 3 {
		t.Fatalf("input = %+v", input)
	}
	reasoning, _ := input[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("reasoning item = %+v", reasoning)
	}
	call, _ := input[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" {
		t.Fatalf("function call = %+v", call)
	}
}
