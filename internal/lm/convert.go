package lm

import (
	"fmt"
	"reflect"
	"strings"
)

func responsesRequestFromChat(target OpenAIResponseRequest, source OpenAIChatRequest) (LMDocument, error) {
	if err := copyDocumentRaw(source, map[string]any(target)); err != nil {
		return nil, err
	}
	rawTarget := map[string]any(target)
	messages, hasMessages := source.Get("messages")
	deleteFields(rawTarget, "messages", "tools", "tool_choice", "max_tokens", "max_completion_tokens", "reasoning_effort", "reasoning_summary", "response_format")
	if hasMessages {
		instructions, input := messagesToResponsesInput(messages)
		if instructions != "" {
			rawTarget["instructions"] = instructions
		}
		rawTarget["input"] = input
	}
	copyField(rawTarget, source, "tools", chatToolsToResponses)
	copyField(rawTarget, source, "tool_choice", chatToolChoiceToResponses)
	if value, ok := source.Get("max_completion_tokens"); ok {
		rawTarget["max_output_tokens"] = cloneValue(value)
	} else if value, ok := source.Get("max_tokens"); ok {
		rawTarget["max_output_tokens"] = cloneValue(value)
	}
	if value, ok := source.Get("response_format"); ok {
		if text, ok := chatResponseFormatToResponses(value); ok {
			rawTarget["text"] = text
		}
	}
	if value, ok := source.Get("reasoning.effort"); ok {
		if err := target.Set("reasoning.effort", value); err != nil {
			return nil, err
		}
	}
	if value, ok := source.Get("reasoning.summary"); ok {
		if err := target.Set("reasoning.summary", value); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func chatRequestFromResponses(target OpenAIChatRequest, source OpenAIResponseRequest) (LMDocument, error) {
	if value, ok := source.Get("previous_response_id"); ok && value != nil && value != "" {
		return nil, fmt.Errorf("previous_response_id is not supported by openai chat")
	}
	if err := copyDocumentRaw(source, map[string]any(target)); err != nil {
		return nil, err
	}
	rawTarget := map[string]any(target)
	deleteFields(rawTarget, "input", "instructions", "tools", "tool_choice", "reasoning", "max_output_tokens", "text", "previous_response_id")
	input, hasInput := source.Get("input")
	messages, err := responsesInputToMessages(input)
	if err != nil {
		return nil, err
	}
	instructionsText := ""
	if instructions, ok := source.Get("instructions"); ok {
		if text, ok := instructions.(string); ok && text != "" {
			instructionsText = text
			messages = append([]any{map[string]any{"role": "system", "content": text}}, messages...)
		}
	}
	if hasInput || len(messages) > 0 || instructionsText != "" {
		rawTarget["messages"] = messages
	}
	copyField(rawTarget, source, "tools", responsesToolsToChat)
	copyField(rawTarget, source, "tool_choice", responsesToolChoiceToChat)
	if value, ok := source.Get("max_output_tokens"); ok {
		rawTarget["max_tokens"] = cloneValue(value)
	}
	if value, ok := responsesTextToChat(source); ok {
		rawTarget["response_format"] = value
	}
	if value, ok := source.Get("reasoning.effort"); ok {
		rawTarget["reasoning_effort"] = cloneValue(value)
	}
	if value, ok := source.Get("reasoning.summary"); ok {
		rawTarget["reasoning_summary"] = cloneValue(value)
	}
	return target, nil
}

func chatResponseFromResponses(target OpenAIChatResponse, source OpenAIResponseResponse) (LMDocument, error) {
	status, _ := source["status"].(string)
	if status == "failed" {
		return nil, responseFailure(source)
	}
	content, reasoning, calls, refusal := responseOutputToChat(source["output"])
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := responseFinishReason(source, len(calls) > 0)
	target["id"] = source["id"]
	target["object"] = "chat.completion"
	target["created"] = source["created_at"]
	target["model"] = source["model"]
	target["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}
	if usage, ok := source["usage"].(map[string]any); ok {
		target["usage"] = responsesUsageToChat(usage)
	}
	return target, nil
}

func responsesResponseFromChat(target OpenAIResponseResponse, source OpenAIChatResponse) (LMDocument, error) {
	id, _ := source["id"].(string)
	if id == "" {
		id = "resp_lmgateway"
	}
	model, _ := source["model"].(string)
	choices := asSlice(source["choices"])
	if len(choices) > 1 {
		return nil, fmt.Errorf("responses conversion does not support multiple chat choices")
	}
	output := []any{}
	status := "completed"
	var incompleteReason string
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			return nil, fmt.Errorf("chat completion choice has no message")
		}
		if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
			output = append(output, map[string]any{
				"type": "reasoning", "id": id + "_reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
			})
		}
		if content := chatContentToResponses(message["content"]); len(content) > 0 {
			output = append(output, map[string]any{
				"type": "message", "id": id + "_message", "role": "assistant", "content": content,
			})
		}
		if refusal, ok := message["refusal"].(string); ok && refusal != "" {
			output = append(output, map[string]any{
				"type": "message", "id": id + "_message", "role": "assistant",
				"content": []any{map[string]any{"type": "refusal", "refusal": refusal}},
			})
		}
		for _, rawCall := range asSlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			callID, _ := call["id"].(string)
			if callID == "" {
				callID = id + "_call"
			}
			output = append(output, map[string]any{
				"type": "function_call", "id": callID, "call_id": callID,
				"name": fn["name"], "arguments": fn["arguments"],
			})
		}
		finishReason, _ := choice["finish_reason"].(string)
		switch finishReason {
		case "length":
			status, incompleteReason = "incomplete", "max_output_tokens"
		case "content_filter":
			status, incompleteReason = "incomplete", "content_filter"
		case "tool_calls", "stop", "":
		default:
			status, incompleteReason = "incomplete", finishReason
		}
	}
	target["id"] = id
	target["object"] = "response"
	target["model"] = model
	target["status"] = status
	target["output"] = output
	if incompleteReason != "" {
		target["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	if created, ok := source["created"].(any); ok {
		target["created_at"] = created
	}
	if usage, ok := source["usage"].(map[string]any); ok {
		target["usage"] = chatUsageToResponses(usage)
	}
	return target, nil
}

func copyDocumentRaw(source LMDocument, target map[string]any) error {
	raw, ok := Map(source)
	if !ok {
		return fmt.Errorf("cannot read %s document", documentName(source))
	}
	for key, value := range raw {
		target[key] = cloneValue(value)
	}
	return nil
}

func copyField(target map[string]any, source LMDocument, path string, convert func(any) any) {
	if value, ok := source.Get(path); ok {
		target[path] = cloneValue(convert(value))
	}
}

func deleteFields(raw map[string]any, fields ...string) {
	for _, field := range fields {
		delete(raw, field)
	}
}

func messagesToResponsesInput(raw any) (string, []any) {
	instructions, input := []string{}, []any{}
	for _, rawMessage := range asSlice(raw) {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		content := message["content"]
		if role == "system" || role == "developer" {
			if text := contentText(content); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if role == "tool" {
			callID, _ := message["tool_call_id"].(string)
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": contentText(content)})
			continue
		}
		input = append(input, map[string]any{"role": role, "content": responseContent(content, role)})
		if role == "assistant" {
			for _, rawCall := range asSlice(message["tool_calls"]) {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
			}
		}
	}
	return joinInstructions(instructions), input
}

func joinInstructions(values []string) string {
	return strings.Join(values, "\n\n")
}

func responsesInputToMessages(raw any) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	messages := []any{}
	for _, rawItem := range asSlice(raw) {
		if text, ok := rawItem.(string); ok {
			messages = append(messages, map[string]any{"role": "user", "content": text})
			continue
		}
		item, _ := rawItem.(map[string]any)
		typ, _ := item["type"].(string)
		switch typ {
		case "function_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]},
			}}})
		case "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": item["output"]})
		case "reasoning":
			if text := contentText(item["summary"]); text != "" {
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "reasoning_content": text})
			}
		default:
			role, _ := item["role"].(string)
			if role != "" {
				messages = append(messages, map[string]any{"role": role, "content": responsesContentToChat(item["content"])})
			} else if typ != "" {
				messages = append(messages, map[string]any{"role": "user", "content": responsesContentToChat([]any{item})})
			}
		}
	}
	return messages, nil
}

func responseContent(raw any, role string) []any {
	if text, ok := raw.(string); ok {
		typ := "input_text"
		if role == "assistant" {
			typ = "output_text"
		}
		return []any{map[string]any{"type": typ, "text": text}}
	}
	if part, ok := raw.(map[string]any); ok {
		raw = []any{part}
	}
	out := []any{}
	for _, rawPart := range asSlice(raw) {
		part, _ := rawPart.(map[string]any)
		typ, _ := part["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			mapped := "input_text"
			if role == "assistant" {
				mapped = "output_text"
			}
			out = append(out, map[string]any{"type": mapped, "text": part["text"]})
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			out = append(out, map[string]any{"type": "input_image", "image_url": image["url"]})
		default:
			out = append(out, cloneValue(part))
		}
	}
	return out
}

func responsesContentToChat(raw any) any {
	if text, ok := raw.(string); ok {
		return text
	}
	parts := asSlice(raw)
	if len(parts) == 0 {
		return []any{}
	}
	out := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": part["text"]})
		case "input_image":
			imageURL := part["image_url"]
			if image, ok := imageURL.(map[string]any); ok {
				imageURL = image["url"]
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
		default:
			out = append(out, cloneValue(part))
		}
	}
	return out
}

func chatContentToResponses(raw any) []any {
	if raw == nil {
		return nil
	}
	return responseContent(raw, "assistant")
}

func contentText(raw any) string {
	if text, ok := raw.(string); ok {
		return text
	}
	parts := []string{}
	if object, ok := raw.(map[string]any); ok {
		if text, ok := object["text"].(string); ok {
			parts = append(parts, text)
		}
		if nested, ok := object["content"]; ok {
			if text := contentText(nested); text != "" {
				parts = append(parts, text)
			}
		}
	}
	for _, rawPart := range asSlice(raw) {
		part, _ := rawPart.(map[string]any)
		if text, ok := part["text"].(string); ok {
			parts = append(parts, text)
		} else if nested, ok := part["content"]; ok {
			if text := contentText(nested); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return joinInstructions(parts)
}

func chatToolsToResponses(raw any) any {
	out := []any{}
	for _, rawTool := range asSlice(raw) {
		tool, _ := rawTool.(map[string]any)
		if tool["type"] != "function" {
			out = append(out, cloneValue(tool))
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		item := map[string]any{"type": "function"}
		for _, key := range []string{"name", "description", "parameters"} {
			if value, ok := fn[key]; ok {
				item[key] = cloneValue(value)
			}
		}
		if strict, ok := fn["strict"]; ok {
			item["strict"] = cloneValue(strict)
		}
		out = append(out, item)
	}
	return out
}

func responsesToolsToChat(raw any) any {
	out := []any{}
	for _, rawTool := range asSlice(raw) {
		tool, _ := rawTool.(map[string]any)
		if tool["type"] != "function" {
			out = append(out, cloneValue(tool))
			continue
		}
		fn := map[string]any{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				fn[key] = cloneValue(value)
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func chatToolChoiceToResponses(raw any) any {
	choice, ok := raw.(map[string]any)
	if !ok || choice["type"] != "function" {
		return cloneValue(raw)
	}
	fn, _ := choice["function"].(map[string]any)
	return map[string]any{"type": "function", "name": fn["name"]}
}

func responsesToolChoiceToChat(raw any) any {
	choice, ok := raw.(map[string]any)
	if !ok || choice["type"] != "function" {
		return cloneValue(raw)
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
}

func chatResponseFormatToResponses(raw any) (map[string]any, bool) {
	format, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]any{}
	typ, _ := format["type"].(string)
	if typ == "json_schema" {
		if schema, ok := format["json_schema"].(map[string]any); ok {
			for _, key := range []string{"name", "description", "schema", "strict"} {
				if value, ok := schema[key]; ok {
					out[key] = cloneValue(value)
				}
			}
		}
		out["type"] = "json_schema"
	} else if typ != "" {
		out["type"] = typ
		for key, value := range format {
			if key != "type" {
				out[key] = cloneValue(value)
			}
		}
	} else {
		return nil, false
	}
	return map[string]any{"format": out}, true
}

func responsesTextToChat(source OpenAIResponseRequest) (any, bool) {
	text, ok := source.Get("text")
	if !ok {
		return nil, false
	}
	textObject, ok := text.(map[string]any)
	if !ok {
		return nil, false
	}
	format, ok := textObject["format"].(map[string]any)
	if !ok {
		return nil, false
	}
	typ, _ := format["type"].(string)
	if typ != "json_schema" {
		out := map[string]any{"type": typ}
		for key, value := range format {
			if key != "type" {
				out[key] = cloneValue(value)
			}
		}
		return out, true
	}
	schema := map[string]any{}
	for _, key := range []string{"name", "description", "schema", "strict"} {
		if value, ok := format[key]; ok {
			schema[key] = cloneValue(value)
		}
	}
	return map[string]any{"type": "json_schema", "json_schema": schema}, true
}

func responseOutputToChat(raw any) (string, string, []any, string) {
	content, reasoning, refusal, calls := "", "", "", []any{}
	for _, rawItem := range asSlice(raw) {
		item, _ := rawItem.(map[string]any)
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			content += contentText(item["content"])
		case "reasoning":
			reasoning += contentText(item["summary"])
		case "function_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			calls = append(calls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
		case "refusal":
			refusal, _ = item["refusal"].(string)
		}
	}
	return content, reasoning, calls, refusal
}

func responseFinishReason(source OpenAIResponseResponse, hasCalls bool) string {
	if hasCalls {
		return "tool_calls"
	}
	status, _ := source["status"].(string)
	if status == "incomplete" {
		details, _ := source["incomplete_details"].(map[string]any)
		reason, _ := details["reason"].(string)
		switch reason {
		case "content_filter":
			return "content_filter"
		case "max_output_tokens", "length", "":
			return "length"
		default:
			return reason
		}
	}
	return "stop"
}

func responseFailure(source OpenAIResponseResponse) error {
	if value, ok := source["error"].(map[string]any); ok {
		if message, ok := value["message"].(string); ok && message != "" {
			return fmt.Errorf("responses request failed: %s", message)
		}
	}
	return fmt.Errorf("responses request failed")
}

func responsesUsageToChat(usage map[string]any) map[string]any {
	out := map[string]any{}
	if value, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = cloneValue(value)
	}
	if value, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = cloneValue(value)
	}
	if value, ok := usage["total_tokens"]; ok {
		out["total_tokens"] = cloneValue(value)
	}
	if value, ok := usage["input_tokens_details"]; ok {
		out["prompt_tokens_details"] = cloneValue(value)
	}
	if value, ok := usage["output_tokens_details"]; ok {
		out["completion_tokens_details"] = cloneValue(value)
	}
	return out
}

func chatUsageToResponses(usage map[string]any) map[string]any {
	out := map[string]any{}
	if value, ok := usage["prompt_tokens"]; ok {
		out["input_tokens"] = cloneValue(value)
	}
	if value, ok := usage["completion_tokens"]; ok {
		out["output_tokens"] = cloneValue(value)
	}
	if value, ok := usage["total_tokens"]; ok {
		out["total_tokens"] = cloneValue(value)
	}
	if value, ok := usage["prompt_tokens_details"]; ok {
		out["input_tokens_details"] = cloneValue(value)
	}
	if value, ok := usage["completion_tokens_details"]; ok {
		out["output_tokens_details"] = cloneValue(value)
	}
	return out
}

func asSlice(raw any) []any {
	if raw == nil {
		return nil
	}
	if values, ok := raw.([]any); ok {
		return values
	}
	value := reflect.ValueOf(raw)
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return nil
	}
	out := make([]any, value.Len())
	for i := range out {
		out[i] = value.Index(i).Interface()
	}
	return out
}
