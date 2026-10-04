package lm

import (
	"context"
	"fmt"
)

type responseEventAdapter struct {
	sourceType       string
	targetType       string
	responseID       string
	model            string
	responseStarted  bool
	terminalSeen     bool
	roleSent         bool
	messageStarted   bool
	reasoningStarted bool
	messageID        string
	reasoningID      string
	toolIndexes      map[string]int
	toolCalls        map[int]map[string]any
	nextTool         int
	finish           string
	usage            map[string]any
	output           []any
}

func newResponseEventAdapter(sourceType, targetType string) *responseEventAdapter {
	return &responseEventAdapter{
		sourceType:  sourceType,
		targetType:  targetType,
		toolIndexes: map[string]int{},
		toolCalls:   map[int]map[string]any{},
	}
}

func adaptResponseEvents(target LMDocument, sourceType, targetType string, input <-chan LMEvent, ctx context.Context) (LMResponse, error) {
	if input == nil {
		return nil, fmt.Errorf("source response event channel is nil")
	}
	response := newStreamResponse(targetType, target, ctx)
	output := make(chan LMEvent)
	response.events = output
	adapter := newResponseEventAdapter(sourceType, targetType)
	go func() {
		defer close(output)
		for {
			select {
			case <-response.ctx.Done():
				return
			case event, ok := <-input:
				if !ok {
					for _, converted := range adapter.flush() {
						if !emitResponseEvent(response, output, converted) {
							return
						}
					}
					return
				}
				converted, err := adapter.convert(event)
				if err != nil {
					errEvent := streamErrorEvent(targetType, err)
					emitResponseEvent(response, output, errEvent)
					return
				}
				for _, targetEvent := range converted {
					if !emitResponseEvent(response, output, targetEvent) {
						return
					}
				}
			}
		}
	}()
	return response, nil
}

func emitResponseEvent(response *StreamResponse, output chan<- LMEvent, event LMEvent) bool {
	if err := ApplyEvent(response.document, event); err != nil {
		return false
	}
	return sendEvent(response.ctx, output, event)
}

func (a *responseEventAdapter) convert(event LMEvent) ([]LMEvent, error) {
	if a.sourceType == a.targetType {
		return []LMEvent{event}, nil
	}
	switch {
	case a.sourceType == "openai" && a.targetType == "openai_response":
		return a.chatToResponses(event)
	case a.sourceType == "openai_response" && a.targetType == "openai":
		return a.responsesToChat(event)
	default:
		return nil, fmt.Errorf("unsupported streaming conversion %s -> %s", a.sourceType, a.targetType)
	}
}

func (a *responseEventAdapter) flush() []LMEvent {
	if a.sourceType == "openai" && a.targetType == "openai_response" {
		return a.flushChatToResponses()
	}
	if a.sourceType == "openai_response" && a.targetType == "openai" && !a.terminalSeen {
		return []LMEvent{a.chatChunk(map[string]any{}, "stop")}
	}
	return nil
}

func (a *responseEventAdapter) responsesToChat(event LMEvent) ([]LMEvent, error) {
	raw, ok := MapEvent(event)
	if !ok {
		return nil, fmt.Errorf("unsupported Responses event %T", event)
	}
	typ := eventType(raw)
	if response, ok := raw["response"].(map[string]any); ok {
		a.updateResponse(response)
	}
	if a.responseID == "" {
		a.responseID = "resp_lmgateway"
	}
	if typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed" || typ == "error" {
		a.terminalSeen = true
	}
	if typ == "response.completed" || typ == "response.incomplete" {
		if response, ok := raw["response"].(map[string]any); ok {
			if output := asSlice(response["output"]); len(output) > 0 && !a.hasChatOutput() {
				converted, err := a.outputToChat(output)
				if err != nil {
					return nil, err
				}
				converted = append(converted, a.chatChunk(map[string]any{}, a.responseFinish(typ == "response.incomplete")))
				if a.usage != nil {
					converted = append(converted, a.usageChunk())
				}
				return converted, nil
			}
		}
	}
	switch typ {
	case "response.output_text.delta":
		delta := map[string]any{"content": eventString(raw["delta"])}
		if !a.roleSent {
			delta["role"] = "assistant"
			a.roleSent = true
		}
		return []LMEvent{a.chatChunk(delta, nil)}, nil
	case "response.reasoning_text.delta", "response.reasoning_summary.delta", "response.reasoning_summary_text.delta":
		return []LMEvent{a.chatChunk(map[string]any{"reasoning_content": eventString(raw["delta"])}, nil)}, nil
	case "response.output_item.added":
		item, _ := raw["item"].(map[string]any)
		if eventString(item["type"]) != "function_call" {
			return nil, nil
		}
		itemID := eventString(item["id"])
		callID := eventString(item["call_id"])
		idx := a.ensureTool(itemID, callID, eventString(item["name"]))
		call := a.toolCalls[idx]
		args := eventString(item["arguments"])
		if args != "" {
			call["arguments"] = args
		}
		return []LMEvent{a.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": eventString(call["call_id"]), "type": "function",
			"function": map[string]any{"name": eventString(call["name"]), "arguments": args},
		}}}, nil)}, nil
	case "response.function_call_arguments.delta":
		id := firstEventString(raw, "item_id", "call_id")
		idx := a.ensureTool(id, id, "")
		call := a.toolCalls[idx]
		args := eventString(raw["delta"])
		call["arguments"] = eventString(call["arguments"]) + args
		return []LMEvent{a.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": eventString(call["call_id"]), "type": "function",
			"function": map[string]any{"name": eventString(call["name"]), "arguments": args},
		}}}, nil)}, nil
	case "response.completed":
		out := []LMEvent{a.chatChunk(map[string]any{}, a.responseFinish(false))}
		if a.usage != nil {
			out = append(out, a.usageChunk())
		}
		return out, nil
	case "response.incomplete":
		out := []LMEvent{a.chatChunk(map[string]any{}, a.responseFinish(true))}
		if a.usage != nil {
			out = append(out, a.usageChunk())
		}
		return out, nil
	case "response.failed", "error":
		return []LMEvent{OpenAIChatEvent{"type": "error", "error": cloneValue(raw["error"])}}, nil
	default:
		return nil, nil
	}
}

func (a *responseEventAdapter) chatToResponses(event LMEvent) ([]LMEvent, error) {
	raw, ok := MapEvent(event)
	if !ok {
		return nil, fmt.Errorf("unsupported Chat event %T", event)
	}
	if eventType(raw) == "error" {
		return []LMEvent{responsesEvent("error", map[string]any{"error": cloneValue(raw["error"])})}, nil
	}
	if id := eventString(raw["id"]); id != "" {
		a.responseID = id
	}
	if model := eventString(raw["model"]); model != "" {
		a.model = model
	}
	if a.responseID == "" {
		a.responseID = "resp_lmgateway"
	}
	out := []LMEvent{}
	if !a.responseStarted {
		a.responseStarted = true
		response := map[string]any{"id": a.responseID, "object": "response", "status": "in_progress"}
		if a.model != "" {
			response["model"] = a.model
		}
		out = append(out, responsesEvent("response.created", map[string]any{"response": response}))
	}
	choices := asSlice(raw["choices"])
	if len(choices) == 0 {
		if usage, ok := raw["usage"].(map[string]any); ok {
			a.usage = cloneMap(usage)
		}
		return out, nil
	}
	choice, _ := choices[0].(map[string]any)
	if reason := eventString(choice["finish_reason"]); reason != "" {
		a.finish = reason
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return out, nil
	}
	if reasoning := eventString(delta["reasoning_content"]); reasoning != "" {
		if !a.reasoningStarted {
			a.reasoningStarted = true
			a.reasoningID = a.responseID + "_reasoning"
			item := map[string]any{"type": "reasoning", "id": a.reasoningID, "summary": []any{map[string]any{"type": "summary_text", "text": ""}}}
			a.output = append(a.output, item)
			out = append(out,
				responsesEvent("response.output_item.added", map[string]any{"item": cloneMap(item), "output_index": len(a.output) - 1}),
				responsesEvent("response.reasoning_summary_part.added", map[string]any{"item_id": a.reasoningID, "summary_index": 0}),
			)
		}
		idx := a.outputIndexByID(a.reasoningID)
		item := a.output[idx].(map[string]any)
		part := asSlice(item["summary"])[0].(map[string]any)
		part["text"] = eventString(part["text"]) + reasoning
		out = append(out, responsesEvent("response.reasoning_summary_text.delta", map[string]any{"item_id": a.reasoningID, "summary_index": 0, "delta": reasoning}))
	}
	if content := eventString(delta["content"]); content != "" {
		if !a.messageStarted {
			a.messageStarted = true
			a.messageID = a.responseID + "_message"
			item := map[string]any{"type": "message", "id": a.messageID, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": ""}}}
			a.output = append(a.output, item)
			out = append(out,
				responsesEvent("response.output_item.added", map[string]any{"item": cloneMap(item), "output_index": len(a.output) - 1}),
				responsesEvent("response.content_part.added", map[string]any{"item_id": a.messageID, "output_index": len(a.output) - 1, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}}),
			)
		}
		idx := a.outputIndexByID(a.messageID)
		item := a.output[idx].(map[string]any)
		part := asSlice(item["content"])[0].(map[string]any)
		part["text"] = eventString(part["text"]) + content
		out = append(out, responsesEvent("response.output_text.delta", map[string]any{"item_id": a.messageID, "output_index": idx, "content_index": 0, "delta": content}))
	}
	for _, rawCall := range asSlice(delta["tool_calls"]) {
		call, _ := rawCall.(map[string]any)
		idx := numberValue(call["index"], a.nextTool)
		if idx >= a.nextTool {
			a.nextTool = idx + 1
		}
		item := a.toolCalls[idx]
		if item == nil {
			callID := eventString(call["id"])
			if callID == "" {
				callID = fmt.Sprintf("%s_call_%d", a.responseID, idx)
			}
			name := ""
			if fn, ok := call["function"].(map[string]any); ok {
				name = eventString(fn["name"])
			}
			item = map[string]any{"type": "function_call", "id": fmt.Sprintf("%s_function_%d", a.responseID, idx), "call_id": callID, "name": name, "arguments": ""}
			a.toolCalls[idx] = item
			a.output = append(a.output, item)
			out = append(out, responsesEvent("response.output_item.added", map[string]any{"item": cloneMap(item), "output_index": len(a.output) - 1}))
		}
		if fn, ok := call["function"].(map[string]any); ok {
			if name := eventString(fn["name"]); name != "" {
				item["name"] = name
			}
			if args := eventString(fn["arguments"]); args != "" {
				item["arguments"] = eventString(item["arguments"]) + args
				out = append(out, responsesEvent("response.function_call_arguments.delta", map[string]any{"item_id": item["id"], "output_index": a.outputIndex(item), "delta": args}))
			}
		}
	}
	return out, nil
}

func (a *responseEventAdapter) flushChatToResponses() []LMEvent {
	if !a.responseStarted {
		a.responseStarted = true
		if a.responseID == "" {
			a.responseID = "resp_lmgateway"
		}
	}
	out := []LMEvent{}
	for index, rawItem := range a.output {
		item, _ := rawItem.(map[string]any)
		switch eventString(item["type"]) {
		case "reasoning":
			out = append(out,
				responsesEvent("response.reasoning_summary_text.done", map[string]any{"item_id": item["id"], "summary_index": 0}),
				responsesEvent("response.reasoning_summary_part.done", map[string]any{"item_id": item["id"], "summary_index": 0}),
				responsesEvent("response.output_item.done", map[string]any{"item": cloneMap(item), "output_index": index}),
			)
		case "message":
			content := asSlice(item["content"])
			part := map[string]any{"type": "output_text", "text": ""}
			if len(content) > 0 {
				if value, ok := content[0].(map[string]any); ok {
					part = value
				}
			}
			out = append(out,
				responsesEvent("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "text": eventString(part["text"])}),
				responsesEvent("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "part": cloneMap(part)}),
				responsesEvent("response.output_item.done", map[string]any{"item": cloneMap(item), "output_index": index}),
			)
		case "function_call":
			out = append(out, responsesEvent("response.output_item.done", map[string]any{"item": cloneMap(item), "output_index": index}))
		}
	}
	status := "completed"
	terminal := "response.completed"
	incompleteReason := ""
	switch a.finish {
	case "length":
		status, terminal, incompleteReason = "incomplete", "response.incomplete", "max_output_tokens"
	case "content_filter":
		status, terminal, incompleteReason = "incomplete", "response.incomplete", "content_filter"
	case "", "stop", "tool_calls":
	default:
		status, terminal, incompleteReason = "incomplete", "response.incomplete", a.finish
	}
	response := map[string]any{"id": a.responseID, "object": "response", "status": status, "model": a.model, "output": cloneValue(a.output)}
	if a.usage != nil {
		response["usage"] = chatUsageToResponses(a.usage)
	}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	out = append(out, responsesEvent(terminal, map[string]any{"response": response}))
	return out
}

func (a *responseEventAdapter) outputToChat(output []any) ([]LMEvent, error) {
	out := []LMEvent{}
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		switch eventString(item["type"]) {
		case "reasoning":
			if text := contentText(item["summary"]); text != "" {
				out = append(out, a.chatChunk(map[string]any{"reasoning_content": text}, nil))
			}
		case "message":
			if text := contentText(item["content"]); text != "" {
				out = append(out, a.chatChunk(map[string]any{"content": text}, nil))
			}
		case "function_call":
			callID := eventString(item["call_id"])
			idx := a.ensureTool(eventString(item["id"]), callID, eventString(item["name"]))
			out = append(out, a.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": callID, "type": "function", "function": map[string]any{"name": eventString(item["name"]), "arguments": eventString(item["arguments"])},
			}}}, nil))
		}
	}
	return out, nil
}

func (a *responseEventAdapter) hasChatOutput() bool {
	return a.roleSent || len(a.toolCalls) > 0
}

func (a *responseEventAdapter) updateResponse(response map[string]any) {
	if id := eventString(response["id"]); id != "" {
		a.responseID = id
	}
	if model := eventString(response["model"]); model != "" {
		a.model = model
	}
	if usage, ok := response["usage"].(map[string]any); ok {
		a.usage = cloneMap(usage)
	}
	if output, ok := response["output"].([]any); ok && len(output) > 0 {
		a.roleSent = true
	}
	if status := eventString(response["status"]); status == "incomplete" {
		if details, ok := response["incomplete_details"].(map[string]any); ok {
			a.finish = eventString(details["reason"])
		}
	}
}

func (a *responseEventAdapter) chatChunk(delta map[string]any, finish any) LMEvent {
	return OpenAIChatEvent(map[string]any{
		"id": a.responseID, "object": "chat.completion.chunk", "model": a.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

func (a *responseEventAdapter) usageChunk() LMEvent {
	return OpenAIChatEvent(map[string]any{
		"id": a.responseID, "object": "chat.completion.chunk", "model": a.model,
		"choices": []any{}, "usage": responsesUsageToChat(a.usage),
	})
}

func (a *responseEventAdapter) responseFinish(incomplete bool) string {
	if len(a.toolCalls) > 0 {
		return "tool_calls"
	}
	if !incomplete {
		return "stop"
	}
	if a.finish == "content_filter" {
		return "content_filter"
	}
	return "length"
}

func (a *responseEventAdapter) ensureTool(itemID, callID, name string) int {
	if itemID != "" {
		if idx, ok := a.toolIndexes[itemID]; ok {
			if name != "" {
				a.toolCalls[idx]["name"] = name
			}
			return idx
		}
	}
	if callID != "" {
		if idx, ok := a.toolIndexes[callID]; ok {
			if itemID != "" {
				a.toolIndexes[itemID] = idx
			}
			if name != "" {
				a.toolCalls[idx]["name"] = name
			}
			return idx
		}
	}
	idx := a.nextTool
	a.nextTool++
	if callID == "" {
		callID = itemID
	}
	a.toolCalls[idx] = map[string]any{"id": callID, "call_id": callID, "name": name, "arguments": "", "type": "function_call"}
	if itemID != "" {
		a.toolIndexes[itemID] = idx
	}
	if callID != "" {
		a.toolIndexes[callID] = idx
	}
	return idx
}

func (a *responseEventAdapter) outputIndexByID(id string) int {
	for i, raw := range a.output {
		item, _ := raw.(map[string]any)
		if eventString(item["id"]) == id {
			return i
		}
	}
	return -1
}

func (a *responseEventAdapter) outputIndex(item map[string]any) int {
	return a.outputIndexByID(eventString(item["id"]))
}

func eventType(raw map[string]any) string {
	if name, ok := raw[eventNameKey].(string); ok && name != "" {
		return name
	}
	return eventString(raw["type"])
}

func eventString(value any) string {
	text, _ := value.(string)
	return text
}

func firstEventString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := eventString(raw[key]); value != "" {
			return value
		}
	}
	return ""
}

func responsesEvent(name string, fields map[string]any) LMEvent {
	body := map[string]any{eventNameKey: name, "type": name}
	for key, value := range fields {
		body[key] = value
	}
	return OpenAIResponsesEvent(body)
}

func ApplyEvent(document LMDocument, event LMEvent) error {
	switch event := event.(type) {
	case OpenAIChatEvent:
		return applyChatEvent(document, event)
	case OpenAIResponsesEvent:
		return applyResponsesEvent(document, event)
	default:
		return fmt.Errorf("unsupported event type %T", event)
	}
}

func applyChatEvent(document LMDocument, event OpenAIChatEvent) error {
	raw := map[string]any(event)
	if eventType(raw) == "error" {
		return document.Set("error", cloneValue(raw["error"]))
	}
	if id := eventString(raw["id"]); id != "" {
		_ = document.Set("id", id)
	}
	_ = document.Set("object", "chat.completion")
	if model := eventString(raw["model"]); model != "" {
		_ = document.Set("model", model)
	}
	if usage, ok := raw["usage"].(map[string]any); ok && len(usage) > 0 {
		if err := document.Set("usage", cloneMap(usage)); err != nil {
			return err
		}
	}
	choices := asSlice(raw["choices"])
	for index, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		if logprobs, ok := choice["logprobs"].(map[string]any); ok && len(logprobs) > 0 {
			if err := appendChatLogprobs(document, index, logprobs); err != nil {
				return err
			}
		}
	}
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if finish := eventString(choice["finish_reason"]); finish != "" {
		list := ensureChatChoices(document)
		list[0].(map[string]any)["finish_reason"] = finish
		if err := document.Set("choices", list); err != nil {
			return err
		}
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta != nil {
		return applyChatDelta(document, delta)
	}
	return nil
}

func ensureChatChoices(document LMDocument) []any {
	choices, _ := document.Get("choices")
	list := asSlice(choices)
	if len(list) == 0 {
		list = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}
	}
	return list
}

// appendChatLogprobs merges one chunk's logprobs into the accumulated response
// document so response-stage handlers can read the full token-level data.
func appendChatLogprobs(document LMDocument, index int, logprobs map[string]any) error {
	choices := ensureChatChoices(document)
	for len(choices) <= index {
		choices = append(choices, map[string]any{
			"index":         len(choices),
			"message":       map[string]any{"role": "assistant", "content": ""},
			"finish_reason": nil,
		})
	}
	choice, _ := choices[index].(map[string]any)
	if choice == nil {
		choice = map[string]any{"index": index}
		choices[index] = choice
	}
	existing, _ := choice["logprobs"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for _, key := range []string{"content", "refusal", "reasoning_content"} {
		entries, ok := logprobs[key].([]any)
		if !ok || len(entries) == 0 {
			continue
		}
		merged := asSlice(existing[key])
		merged = append(merged, entries...)
		existing[key] = merged
	}
	choice["logprobs"] = existing
	return document.Set("choices", choices)
}

func applyChatDelta(document LMDocument, delta map[string]any) error {
	choices := ensureChatChoices(document)
	choice := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{"role": "assistant", "content": ""}
		choice["message"] = message
	}
	if role := eventString(delta["role"]); role != "" {
		message["role"] = role
	}
	if content := eventString(delta["content"]); content != "" {
		message["content"] = eventString(message["content"]) + content
	}
	if reasoning := eventString(delta["reasoning_content"]); reasoning != "" {
		message["reasoning_content"] = eventString(message["reasoning_content"]) + reasoning
	}
	for _, rawCall := range asSlice(delta["tool_calls"]) {
		call, _ := rawCall.(map[string]any)
		calls := asSlice(message["tool_calls"])
		idx := numberValue(call["index"], len(calls))
		for len(calls) <= idx {
			calls = append(calls, map[string]any{"index": len(calls), "type": "function", "function": map[string]any{"name": "", "arguments": ""}})
		}
		assembled, _ := calls[idx].(map[string]any)
		if id := eventString(call["id"]); id != "" {
			assembled["id"] = id
		}
		fn, _ := call["function"].(map[string]any)
		assembledFn, _ := assembled["function"].(map[string]any)
		if name := eventString(fn["name"]); name != "" {
			assembledFn["name"] = name
		}
		if args := eventString(fn["arguments"]); args != "" {
			assembledFn["arguments"] = eventString(assembledFn["arguments"]) + args
		}
		message["tool_calls"] = calls
	}
	return document.Set("choices", choices)
}

func applyResponsesEvent(document LMDocument, event OpenAIResponsesEvent) error {
	raw := map[string]any(event)
	_ = document.Set("object", "response")
	if response, ok := raw["response"].(map[string]any); ok {
		for key, value := range response {
			if value == nil {
				continue
			}
			if err := document.Set(key, cloneValue(value)); err != nil {
				return err
			}
		}
	}
	if eventType(raw) == "error" {
		return document.Set("error", cloneValue(raw["error"]))
	}
	if id := eventString(raw["id"]); id != "" {
		_ = document.Set("id", id)
	}
	if model := eventString(raw["model"]); model != "" {
		_ = document.Set("model", model)
	}
	typ := eventType(raw)
	if typ == "response.output_item.added" || typ == "response.output_item.done" {
		item, _ := raw["item"].(map[string]any)
		return upsertResponseOutput(document, item)
	}
	itemID := firstEventString(raw, "item_id", "call_id")
	kind := "message"
	switch typ {
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		kind = "reasoning"
	case "response.function_call_arguments.delta":
		kind = "function_call"
	}
	_, item := ensureResponseOutputItem(document, itemID, kind)
	if item == nil {
		return nil
	}
	switch typ {
	case "response.output_text.delta":
		parts := asSlice(item["content"])
		contentIndex := numberValue(raw["content_index"], 0)
		for len(parts) <= contentIndex {
			parts = append(parts, map[string]any{"type": "output_text", "text": ""})
		}
		part, _ := parts[contentIndex].(map[string]any)
		part["text"] = eventString(part["text"]) + eventString(raw["delta"])
		item["content"] = parts
	case "response.content_part.added", "response.content_part.done":
		if part, ok := raw["part"].(map[string]any); ok {
			parts := asSlice(item["content"])
			contentIndex := numberValue(raw["content_index"], 0)
			for len(parts) <= contentIndex {
				parts = append(parts, map[string]any{})
			}
			parts[contentIndex] = cloneMap(part)
			item["content"] = parts
		}
	case "response.reasoning_summary_text.delta":
		summary := asSlice(item["summary"])
		summaryIndex := numberValue(raw["summary_index"], 0)
		for len(summary) <= summaryIndex {
			summary = append(summary, map[string]any{"type": "summary_text", "text": ""})
		}
		part, _ := summary[summaryIndex].(map[string]any)
		part["text"] = eventString(part["text"]) + eventString(raw["delta"])
		item["summary"] = summary
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if part, ok := raw["part"].(map[string]any); ok {
			summary := asSlice(item["summary"])
			idx := numberValue(raw["summary_index"], 0)
			for len(summary) <= idx {
				summary = append(summary, map[string]any{})
			}
			summary[idx] = cloneMap(part)
			item["summary"] = summary
		}
	case "response.function_call_arguments.delta":
		item["arguments"] = eventString(item["arguments"]) + eventString(raw["delta"])
	}
	return document.Set("output", asSliceValue(document, "output"))
}

func upsertResponseOutput(document LMDocument, item map[string]any) error {
	if item == nil {
		return nil
	}
	output := asSliceValue(document, "output")
	id := eventString(item["id"])
	for i, raw := range output {
		current, _ := raw.(map[string]any)
		if id != "" && eventString(current["id"]) == id {
			output[i] = mergeMap(current, item)
			return document.Set("output", output)
		}
	}
	return document.Set("output", append(output, cloneMap(item)))
}

func ensureResponseOutputItem(document LMDocument, id, kind string) (int, map[string]any) {
	if idx, item := responseOutputItem(document, id); item != nil {
		return idx, item
	}
	if id == "" {
		return -1, nil
	}
	item := map[string]any{"id": id, "type": kind}
	switch kind {
	case "message":
		item["role"] = "assistant"
		item["content"] = []any{}
	case "reasoning":
		item["summary"] = []any{}
	case "function_call":
		item["call_id"] = id
		item["name"] = ""
		item["arguments"] = ""
	}
	output := append(asSliceValue(document, "output"), item)
	_ = document.Set("output", output)
	return len(output) - 1, item
}

func responseOutputItem(document LMDocument, id string) (int, map[string]any) {
	output := asSliceValue(document, "output")
	for i, raw := range output {
		item, _ := raw.(map[string]any)
		if id == "" || eventString(item["id"]) == id || eventString(item["call_id"]) == id {
			return i, item
		}
	}
	return -1, nil
}

func asSliceValue(document LMDocument, path string) []any {
	value, _ := document.Get(path)
	return asSlice(value)
}

func mergeMap(dst, src map[string]any) map[string]any {
	out := cloneMap(dst)
	for key, value := range src {
		out[key] = cloneValue(value)
	}
	return out
}

func numberValue(value any, fallback int) int {
	switch value := value.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return fallback
}
