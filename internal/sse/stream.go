package sse

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type streamMode uint8

const (
	modeOpenAI streamMode = iota
	modeResponses
	modeChatAsResponses
)

type Stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	mode    streamMode
	done    bool
	pending []map[string]any
	events  []Event

	responseID         string
	responseModel      string
	responseModelSet   bool
	responseStarted    bool
	responseCompleted  bool
	responseFinish     string
	reasoningItemID    string
	reasoningStarted   bool
	reasoningOutputIdx int
	messageItemID      string
	messageStarted     bool
	messageOutputIdx   int
	roleSent           bool
	toolIndexes        map[string]int
	toolCallIDs        map[string]string
	toolNames          map[string]string
	nextToolIndex      int
	toolItems          map[int]map[string]any
	toolOutputIndexes  map[int]int
	outputItemIndexes  map[string]int
	responseOutput     []any
	pendingDone        bool

	Usage         map[string]any
	ResponseUsage map[string]any
	Response      map[string]any
}

type Event struct {
	Name string
	Data json.RawMessage
}

func New(body io.ReadCloser) *Stream {
	return newStream(body, modeOpenAI)
}

func NewResponses(body io.ReadCloser) *Stream {
	return newStream(body, modeResponses)
}

func NewNativeResponses(body io.ReadCloser) *Stream {
	return newStream(body, modeResponses)
}

func NewChatAsResponses(body io.ReadCloser) *Stream {
	return newStream(body, modeChatAsResponses)
}

func (s *Stream) SetResponseModel(model string) {
	if model != "" {
		s.responseModel = model
		s.responseModelSet = true
	}
}

func (s *Stream) NextEvent() (Event, error) {
	if s.mode == modeChatAsResponses {
		return s.nextChatAsResponsesEvent()
	}
	if s.done {
		return Event{}, io.EOF
	}
	payload, name, err := s.nextEventPayload()
	if err != nil {
		return Event{}, err
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return Event{}, err
	}
	if name == "" {
		name, _ = event["type"].(string)
	}
	if response, ok := event["response"].(map[string]any); ok {
		if s.responseModelSet && s.responseModel != "" {
			response["model"] = s.responseModel
		}
		s.mergeResponse(response)
	}
	s.recordNativeEvent(name, event)
	if name == "response.completed" || name == "response.incomplete" || name == "response.failed" {
		s.ensureNativeResponse(name)
		s.done = true
	}
	if s.responseModelSet && s.responseModel != "" {
		payloadBytes, marshalErr := json.Marshal(event)
		if marshalErr == nil {
			payload = string(payloadBytes)
		}
	}
	return Event{Name: name, Data: json.RawMessage(payload)}, nil
}

func newStream(body io.ReadCloser, mode streamMode) *Stream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &Stream{
		body:              body,
		scanner:           sc,
		mode:              mode,
		toolIndexes:       map[string]int{},
		toolCallIDs:       map[string]string{},
		toolNames:         map[string]string{},
		toolItems:         map[int]map[string]any{},
		toolOutputIndexes: map[int]int{},
		outputItemIndexes: map[string]int{},
	}
}

func (s *Stream) Next() (map[string]any, error) {
	if len(s.pending) > 0 {
		chunk := s.pending[0]
		s.pending = s.pending[1:]
		return chunk, nil
	}
	if s.done {
		return nil, io.EOF
	}

	for {
		payload, err := s.nextPayload()
		if err != nil {
			return nil, err
		}
		if s.mode == modeResponses {
			chunk, emit, err := s.decodeResponses(payload)
			if err != nil {
				return nil, err
			}
			if !emit {
				if len(s.pending) > 0 {
					chunk := s.pending[0]
					s.pending = s.pending[1:]
					return chunk, nil
				}
				continue
			}
			return chunk, nil
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, err
		}
		if model, ok := chunk["model"].(string); ok && model != "" && !s.responseModelSet {
			s.responseModel = model
		}
		if s.responseModelSet && s.responseModel != "" {
			chunk["model"] = s.responseModel
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			s.Usage = u
		}
		return chunk, nil
	}
}

func (s *Stream) nextChatAsResponsesEvent() (Event, error) {
	for {
		if len(s.events) > 0 {
			event := s.events[0]
			s.events = s.events[1:]
			return event, nil
		}
		if s.responseCompleted {
			return Event{}, io.EOF
		}
		if !s.responseStarted {
			s.responseStarted = true
			if s.responseID == "" {
				s.responseID = fmt.Sprintf("resp_lmgateway_%d", time.Now().UnixNano())
			}
			response := map[string]any{"id": s.responseID, "object": "response", "status": "in_progress"}
			if s.responseModel != "" {
				response["model"] = s.responseModel
			}
			return makeEvent("response.created", map[string]any{"response": response}), nil
		}
		payload, err := s.nextPayload()
		if err == io.EOF {
			s.finishChatAsResponses()
			continue
		}
		if err != nil {
			return Event{}, err
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return Event{}, err
		}
		if model, ok := chunk["model"].(string); ok && model != "" && !s.responseModelSet {
			s.responseModel = model
		}
		if usage, ok := chunk["usage"].(map[string]any); ok {
			s.Usage = usage
			s.ResponseUsage = responsesUsage(usage)
		}
		choices := asSlice(chunk["choices"])
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		s.queueChatDelta(choice)
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			s.responseFinish = reason
		}
		if len(s.events) > 0 {
			continue
		}
	}
}

func (s *Stream) queueChatDelta(choice map[string]any) {
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
		s.ensureReasoning()
		item := s.responseOutput[s.reasoningOutputIdx].(map[string]any)
		summary := asSlice(item["summary"])
		if len(summary) > 0 {
			part, _ := summary[0].(map[string]any)
			text, _ := part["text"].(string)
			part["text"] = text + reasoning
		}
		s.events = append(s.events, makeEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id": s.reasoningItemID, "summary_index": 0, "delta": reasoning,
		}))
	}
	if content, ok := delta["content"].(string); ok && content != "" {
		s.ensureMessage()
		item := s.responseOutput[s.messageOutputIdx].(map[string]any)
		parts := asSlice(item["content"])
		if len(parts) > 0 {
			part, _ := parts[0].(map[string]any)
			text, _ := part["text"].(string)
			part["text"] = text + content
		}
		s.events = append(s.events, makeEvent("response.output_text.delta", map[string]any{
			"item_id": s.messageItemID, "output_index": s.messageOutputIdx, "content_index": 0, "delta": content,
		}))
	}
	for _, rawCall := range asSlice(delta["tool_calls"]) {
		call, _ := rawCall.(map[string]any)
		s.queueChatToolCall(call)
	}
}

func (s *Stream) ensureReasoning() {
	if s.reasoningStarted {
		return
	}
	s.reasoningStarted = true
	s.reasoningItemID = s.responseID + "_reasoning"
	s.reasoningOutputIdx = len(s.responseOutput)
	item := map[string]any{
		"type": "reasoning", "id": s.reasoningItemID,
		"summary": []any{map[string]any{"type": "summary_text", "text": ""}},
	}
	s.responseOutput = append(s.responseOutput, item)
	s.events = append(s.events,
		makeEvent("response.output_item.added", map[string]any{"item": item}),
		makeEvent("response.reasoning_summary_part.added", map[string]any{"item_id": s.reasoningItemID, "summary_index": 0}),
	)
}

func (s *Stream) ensureMessage() {
	if s.messageStarted {
		return
	}
	s.messageStarted = true
	s.messageItemID = s.responseID + "_message"
	s.messageOutputIdx = len(s.responseOutput)
	part := map[string]any{"type": "output_text", "text": ""}
	item := map[string]any{"type": "message", "id": s.messageItemID, "role": "assistant", "content": []any{part}}
	s.responseOutput = append(s.responseOutput, item)
	s.events = append(s.events,
		makeEvent("response.output_item.added", map[string]any{"item": item}),
		makeEvent("response.content_part.added", map[string]any{"item_id": s.messageItemID, "output_index": s.messageOutputIdx, "content_index": 0, "part": part}),
	)
}

func (s *Stream) queueChatToolCall(call map[string]any) {
	idx := numberAsInt(call["index"], s.nextToolIndex)
	if _, ok := s.toolItems[idx]; !ok {
		s.nextToolIndex++
	}
	item := s.toolItems[idx]
	if item == nil {
		callID, _ := call["id"].(string)
		if callID == "" {
			callID = fmt.Sprintf("%s_call_%d", s.responseID, idx)
		}
		itemID := fmt.Sprintf("%s_function_%d", s.responseID, idx)
		name := ""
		if fn, ok := call["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		item = map[string]any{
			"type": "function_call", "id": itemID, "call_id": callID,
			"name": name, "arguments": "",
		}
		s.toolItems[idx] = item
		s.toolOutputIndexes[idx] = len(s.responseOutput)
		s.responseOutput = append(s.responseOutput, item)
		s.events = append(s.events, makeEvent("response.output_item.added", map[string]any{"item": item}))
	}
	fn, _ := call["function"].(map[string]any)
	if name, ok := fn["name"].(string); ok && name != "" {
		item["name"] = name
	}
	if args, ok := fn["arguments"].(string); ok && args != "" {
		previous, _ := item["arguments"].(string)
		item["arguments"] = previous + args
		s.events = append(s.events, makeEvent("response.function_call_arguments.delta", map[string]any{
			"item_id": item["id"], "output_index": s.toolOutputIndexes[idx], "delta": args,
		}))
	}
}

func (s *Stream) finishChatAsResponses() {
	if s.responseCompleted {
		return
	}
	if s.reasoningStarted {
		item := s.responseOutput[s.reasoningOutputIdx].(map[string]any)
		summary := asSlice(item["summary"])
		var text string
		if len(summary) > 0 {
			text, _ = summary[0].(map[string]any)["text"].(string)
		}
		s.events = append(s.events,
			makeEvent("response.reasoning_summary_text.done", map[string]any{"item_id": s.reasoningItemID, "summary_index": 0, "text": text}),
			makeEvent("response.reasoning_summary_part.done", map[string]any{"item_id": s.reasoningItemID, "summary_index": 0}),
			makeEvent("response.output_item.done", map[string]any{"item": item}),
		)
	}
	if s.messageStarted {
		item := s.responseOutput[s.messageOutputIdx].(map[string]any)
		parts := asSlice(item["content"])
		var text string
		if len(parts) > 0 {
			text, _ = parts[0].(map[string]any)["text"].(string)
		}
		s.events = append(s.events,
			makeEvent("response.output_text.done", map[string]any{"item_id": s.messageItemID, "output_index": s.messageOutputIdx, "content_index": 0, "text": text}),
			makeEvent("response.content_part.done", map[string]any{"item_id": s.messageItemID, "output_index": s.messageOutputIdx, "content_index": 0, "part": parts[0]}),
			makeEvent("response.output_item.done", map[string]any{"item": item}),
		)
	}
	indexes := make([]int, 0, len(s.toolItems))
	for idx := range s.toolItems {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		s.events = append(s.events, makeEvent("response.output_item.done", map[string]any{"item": s.toolItems[idx]}))
	}
	status := "completed"
	terminal := "response.completed"
	incompleteReason := ""
	switch s.responseFinish {
	case "length":
		status, terminal, incompleteReason = "incomplete", "response.incomplete", "max_output_tokens"
	case "content_filter":
		status, terminal, incompleteReason = "incomplete", "response.incomplete", "content_filter"
	case "", "stop", "tool_calls":
	default:
		status, terminal, incompleteReason = "incomplete", "response.incomplete", s.responseFinish
	}
	response := map[string]any{
		"id": s.responseID, "object": "response", "status": status,
		"output": s.responseOutput,
	}
	if s.responseModel != "" {
		response["model"] = s.responseModel
	}
	if s.ResponseUsage != nil {
		response["usage"] = s.ResponseUsage
	}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	s.Response = response
	s.responseCompleted = true
	s.done = true
	s.events = append(s.events, makeEvent(terminal, map[string]any{"response": response}))
}

func makeEvent(name string, fields map[string]any) Event {
	body := map[string]any{"type": name}
	for key, value := range fields {
		body[key] = value
	}
	return Event{Name: name, Data: json.RawMessage(mustJSON(body))}
}

func mustJSON(value any) []byte {
	b, _ := json.Marshal(value)
	return b
}

func responsesUsage(usage map[string]any) map[string]any {
	out := map[string]any{}
	if value, ok := usage["prompt_tokens"]; ok {
		out["input_tokens"] = value
	}
	if value, ok := usage["completion_tokens"]; ok {
		out["output_tokens"] = value
	}
	if value, ok := usage["total_tokens"]; ok {
		out["total_tokens"] = value
	}
	if value, ok := usage["prompt_tokens_details"]; ok {
		out["input_tokens_details"] = value
	}
	if value, ok := usage["completion_tokens_details"]; ok {
		out["output_tokens_details"] = value
	}
	return out
}

func (s *Stream) responseUsage() map[string]any {
	if s.ResponseUsage != nil {
		return s.ResponseUsage
	}
	if s.Usage == nil {
		return nil
	}
	return responsesUsage(s.Usage)
}

func (s *Stream) nextPayload() (string, error) {
	payload, _, err := s.nextEventPayload()
	return payload, err
}

func (s *Stream) nextEventPayload() (string, string, error) {
	if s.pendingDone {
		s.pendingDone = false
		s.done = true
		return "", "", io.EOF
	}
	var data strings.Builder
	eventName := ""
	for s.scanner.Scan() {
		line := strings.TrimRight(s.scanner.Text(), "\r")
		if line == "" {
			if data.Len() > 0 {
				break
			}
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				if data.Len() == 0 {
					s.done = true
					return "", eventName, io.EOF
				}
				s.pendingDone = true
				break
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return "", eventName, err
	}
	if data.Len() == 0 {
		s.done = true
		return "", eventName, io.EOF
	}
	return data.String(), eventName, nil
}

func (s *Stream) decodeResponses(payload string) (map[string]any, bool, error) {
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, false, err
	}
	typ, _ := event["type"].(string)
	if response, ok := event["response"].(map[string]any); ok {
		s.mergeResponse(response)
		if id, ok := response["id"].(string); ok && id != "" {
			s.responseID = id
		}
		if model, ok := response["model"].(string); ok && model != "" && !s.responseModelSet {
			s.responseModel = model
		}
		if usage, ok := response["usage"].(map[string]any); ok {
			s.ResponseUsage = usage
			s.Usage = normalizeResponsesUsage(usage)
		}
	}
	base := func(delta map[string]any, finish any) map[string]any {
		model := s.responseModel
		return map[string]any{
			"id": s.responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}

	switch typ {
	case "response.output_text.delta":
		delta := map[string]any{}
		if !s.roleSent {
			delta["role"] = "assistant"
			s.roleSent = true
		}
		delta["content"] = eventString(event, "delta")
		return base(delta, nil), true, nil
	case "response.reasoning_text.delta", "response.reasoning_summary.delta", "response.reasoning_summary_text.delta":
		return base(map[string]any{"reasoning_content": eventString(event, "delta")}, nil), true, nil
	case "response.function_call_arguments.delta":
		eventID := firstString(event, "call_id", "item_id")
		callID := s.toolCallIDs[eventID]
		if callID == "" {
			callID = eventID
		}
		idx := s.toolIndex(callID)
		name := s.toolNames[callID]
		if name == "" {
			name = s.toolNames[eventID]
		}
		return base(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": callID, "type": "function",
			"function": map[string]any{"name": name, "arguments": eventString(event, "delta")},
		}}}, nil), true, nil
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item["type"] != "function_call" {
			return nil, false, nil
		}
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		itemID, _ := item["id"].(string)
		if itemID != "" {
			s.toolCallIDs[itemID] = callID
		}
		idx := s.toolIndex(callID)
		name, _ := item["name"].(string)
		s.toolNames[callID] = name
		if itemID != "" {
			s.toolNames[itemID] = name
		}
		return base(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": callID, "type": "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}}}, nil), true, nil
	case "response.completed":
		s.done = true
		finish := any(responseFinishFromMap(s.Response, len(s.toolIndexes) > 0))
		s.pending = append(s.pending, base(map[string]any{}, finish))
		if s.Usage != nil {
			s.pending = append(s.pending, map[string]any{
				"id": s.responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": s.responseModel, "choices": []any{}, "usage": s.Usage,
			})
		}
		return nil, false, nil
	case "response.incomplete":
		s.done = true
		s.pending = append(s.pending, base(map[string]any{}, responseFinishFromMap(s.Response, false)))
		if s.Usage != nil {
			s.pending = append(s.pending, map[string]any{
				"id": s.responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": s.responseModel, "choices": []any{}, "usage": s.Usage,
			})
		}
		return nil, false, nil
	case "response.failed", "error":
		errObj, _ := event["error"].(map[string]any)
		if msg, _ := errObj["message"].(string); msg != "" {
			return nil, false, errors.New(msg)
		}
		return nil, false, errors.New("responses stream failed")
	default:
		return map[string]any{
			"id": s.responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": s.responseModel, "choices": []any{},
			"extensions": map[string]any{"responses_event": event},
		}, true, nil
	}
}

func (s *Stream) mergeResponse(response map[string]any) {
	if s.Response == nil {
		s.Response = map[string]any{}
	}
	for key, value := range response {
		s.Response[key] = value
	}
	if usage, ok := response["usage"].(map[string]any); ok {
		s.ResponseUsage = usage
		s.Usage = normalizeResponsesUsage(usage)
	}
}

func (s *Stream) recordNativeEvent(name string, event map[string]any) {
	switch name {
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		itemID, _ := item["id"].(string)
		if itemID == "" {
			return
		}
		if idx, ok := s.outputItemIndexes[itemID]; ok {
			s.responseOutput[idx] = cloneMap(item)
			return
		}
		s.outputItemIndexes[itemID] = len(s.responseOutput)
		s.responseOutput = append(s.responseOutput, cloneMap(item))
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		itemID, _ := item["id"].(string)
		if idx, ok := s.outputItemIndexes[itemID]; ok {
			s.responseOutput[idx] = cloneMap(item)
		}
	case "response.content_part.added", "response.content_part.done":
		itemID, _ := event["item_id"].(string)
		idx, ok := s.outputItemIndexes[itemID]
		if !ok {
			return
		}
		item, _ := s.responseOutput[idx].(map[string]any)
		contentIndex := numberAsInt(event["content_index"], 0)
		parts := asSlice(item["content"])
		for len(parts) <= contentIndex {
			parts = append(parts, map[string]any{})
		}
		part, _ := event["part"].(map[string]any)
		if part != nil {
			parts[contentIndex] = cloneMap(part)
		}
		item["content"] = parts
	case "response.output_text.delta":
		itemID, _ := event["item_id"].(string)
		idx, ok := s.outputItemIndexes[itemID]
		if !ok {
			return
		}
		item, _ := s.responseOutput[idx].(map[string]any)
		contentIndex := numberAsInt(event["content_index"], 0)
		parts := asSlice(item["content"])
		for len(parts) <= contentIndex {
			parts = append(parts, map[string]any{"type": "output_text", "text": ""})
		}
		part, _ := parts[contentIndex].(map[string]any)
		text, _ := event["delta"].(string)
		previous, _ := part["text"].(string)
		part["text"] = previous + text
		item["content"] = parts
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		itemID, _ := event["item_id"].(string)
		idx, ok := s.outputItemIndexes[itemID]
		if !ok {
			return
		}
		item, _ := s.responseOutput[idx].(map[string]any)
		summaryIndex := numberAsInt(event["summary_index"], 0)
		summary := asSlice(item["summary"])
		for len(summary) <= summaryIndex {
			summary = append(summary, map[string]any{"type": "summary_text", "text": ""})
		}
		item["summary"] = summary
	case "response.reasoning_summary_text.delta":
		itemID, _ := event["item_id"].(string)
		idx, ok := s.outputItemIndexes[itemID]
		if !ok {
			return
		}
		item, _ := s.responseOutput[idx].(map[string]any)
		summaryIndex := numberAsInt(event["summary_index"], 0)
		summary := asSlice(item["summary"])
		for len(summary) <= summaryIndex {
			summary = append(summary, map[string]any{"type": "summary_text", "text": ""})
		}
		part, _ := summary[summaryIndex].(map[string]any)
		text, _ := event["delta"].(string)
		previous, _ := part["text"].(string)
		part["text"] = previous + text
		item["summary"] = summary
	case "response.function_call_arguments.delta":
		itemID := firstString(event, "item_id", "call_id")
		idx, ok := s.outputItemIndexes[itemID]
		if !ok {
			return
		}
		item, _ := s.responseOutput[idx].(map[string]any)
		text, _ := event["delta"].(string)
		previous, _ := item["arguments"].(string)
		item["arguments"] = previous + text
	}
}

func (s *Stream) ensureNativeResponse(name string) {
	if s.Response == nil {
		s.Response = map[string]any{}
	}
	if _, ok := s.Response["id"]; !ok && s.responseID != "" {
		s.Response["id"] = s.responseID
	}
	if _, ok := s.Response["model"]; !ok && s.responseModel != "" {
		s.Response["model"] = s.responseModel
	}
	if _, ok := s.Response["status"]; !ok {
		s.Response["status"] = map[string]string{
			"response.completed":  "completed",
			"response.incomplete": "incomplete",
			"response.failed":     "failed",
		}[name]
	}
	if _, ok := s.Response["output"]; !ok && len(s.responseOutput) > 0 {
		s.Response["output"] = s.responseOutput
	}
	if s.ResponseUsage != nil {
		s.Response["usage"] = s.ResponseUsage
	}
}

func cloneMap(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		switch value := value.(type) {
		case map[string]any:
			out[key] = cloneMap(value)
		case []any:
			items := make([]any, len(value))
			for i, item := range value {
				if object, ok := item.(map[string]any); ok {
					items[i] = cloneMap(object)
				} else {
					items[i] = item
				}
			}
			out[key] = items
		default:
			out[key] = value
		}
	}
	return out
}

func (s *Stream) toolIndex(id string) int {
	if idx, ok := s.toolIndexes[id]; ok {
		return idx
	}
	idx := s.nextToolIndex
	s.nextToolIndex++
	s.toolIndexes[id] = idx
	return idx
}

func eventString(event map[string]any, key string) string {
	v, _ := event[key].(string)
	return v
}

func firstString(event map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := event[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func numberAsInt(value any, fallback int) int {
	switch value := value.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		if number, err := strconv.Atoi(string(value)); err == nil {
			return number
		}
	}
	return fallback
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

func responseFinishFromMap(response map[string]any, hasCalls bool) string {
	if hasCalls {
		return "tool_calls"
	}
	if response == nil {
		return "stop"
	}
	status, _ := response["status"].(string)
	if status != "incomplete" {
		return "stop"
	}
	details, _ := response["incomplete_details"].(map[string]any)
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

func normalizeResponsesUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	copyNumber(out, u, "input_tokens", "prompt_tokens")
	copyNumber(out, u, "output_tokens", "completion_tokens")
	copyNumber(out, u, "total_tokens", "total_tokens")
	if details, ok := u["output_tokens_details"].(map[string]any); ok {
		if n, ok := details["reasoning_tokens"]; ok {
			out["completion_tokens_details"] = map[string]any{"reasoning_tokens": n}
		}
	}
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		if n, ok := details["cached_tokens"]; ok {
			out["prompt_tokens_details"] = map[string]any{"cached_tokens": n}
		}
	}
	return out
}

func copyNumber(out, in map[string]any, from, to string) {
	if v, ok := in[from]; ok {
		out[to] = v
	}
}

func (s *Stream) Close() error {
	return s.body.Close()
}
