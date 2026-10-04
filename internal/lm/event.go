package lm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const eventNameKey = "__event_name"

type LMEvent interface {
	LMDocument
	eventMarker()
}

type LMResponse interface {
	LMDocument
	responseMarker()
}

type OpenAIChatEvent map[string]any
type OpenAIResponsesEvent map[string]any

type StreamResponse struct {
	document LMDocument
	events   <-chan LMEvent
	ctx      context.Context
	protocol string
}

func (OpenAIChatEvent) eventMarker()      {}
func (OpenAIResponsesEvent) eventMarker() {}
func (*StreamResponse) responseMarker()   {}

func NewChatEvent(raw map[string]any) LMEvent {
	return OpenAIChatEvent(raw)
}

func NewResponsesEvent(raw map[string]any, name string) LMEvent {
	if name != "" {
		raw[eventNameKey] = name
		if _, ok := raw["type"]; !ok {
			raw["type"] = name
		}
	}
	return OpenAIResponsesEvent(raw)
}

func (e OpenAIChatEvent) Get(path string) (any, bool) {
	if path == "type" {
		if value, ok := e["type"]; ok {
			return value, true
		}
		if value, ok := e["object"]; ok {
			return value, true
		}
		return "chat.completion.chunk", true
	}
	return genericGet(map[string]any(e), path, nil)
}

func (e OpenAIChatEvent) Set(path string, value any) error {
	return genericSet(map[string]any(e), path, value)
}

func (e OpenAIChatEvent) SetDefault(path string, value any) error {
	return applyDefault(e, path, value)
}

func (e OpenAIChatEvent) Delete(path string) error {
	return genericDelete(map[string]any(e), path)
}

func (e OpenAIChatEvent) ConvertFrom(src LMDocument) (LMDocument, error) {
	if _, ok := src.(OpenAIChatEvent); ok {
		return src, nil
	}
	return nil, errors.New("cross-protocol event conversion requires LMResponse")
}

func (e OpenAIResponsesEvent) Get(path string) (any, bool) {
	switch path {
	case "type":
		value, ok := e["type"]
		return value, ok
	case "event":
		if value, ok := e[eventNameKey].(string); ok && value != "" {
			return value, true
		}
		value, ok := e["type"].(string)
		return value, ok
	}
	return genericGet(map[string]any(e), path, nil)
}

func (e OpenAIResponsesEvent) Set(path string, value any) error {
	return genericSet(map[string]any(e), path, value)
}

func (e OpenAIResponsesEvent) SetDefault(path string, value any) error {
	return applyDefault(e, path, value)
}

func (e OpenAIResponsesEvent) Delete(path string) error {
	return genericDelete(map[string]any(e), path)
}

func (e OpenAIResponsesEvent) ConvertFrom(src LMDocument) (LMDocument, error) {
	if _, ok := src.(OpenAIResponsesEvent); ok {
		return src, nil
	}
	return nil, errors.New("cross-protocol event conversion requires LMResponse")
}

func NewResponseStream(protocol string, events <-chan LMEvent, ctx context.Context) (LMResponse, error) {
	document, err := NewResponse(protocol, map[string]any{})
	if err != nil {
		return nil, err
	}
	response := newStreamResponse(protocol, document, ctx)
	if events == nil {
		closed := make(chan LMEvent)
		close(closed)
		response.events = closed
		return response, nil
	}
	output := make(chan LMEvent)
	response.events = output
	go func() {
		defer close(output)
		for {
			select {
			case <-response.ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if err := ApplyEvent(response.document, event); err != nil {
					failure := streamErrorEvent(protocol, err)
					_ = ApplyEvent(response.document, failure)
					_ = sendEvent(response.ctx, output, failure)
					return
				}
				if !sendEvent(response.ctx, output, event) {
					return
				}
			}
		}
	}()
	return response, nil
}

func NewResponseTarget(protocol string, ctx context.Context) (LMResponse, error) {
	document, err := NewResponse(protocol, map[string]any{})
	if err != nil {
		return nil, err
	}
	return newStreamResponse(protocol, document, ctx), nil
}

func newStreamResponse(protocol string, document LMDocument, ctx context.Context) *StreamResponse {
	if ctx == nil {
		ctx = context.Background()
	}
	return &StreamResponse{document: document, ctx: ctx, protocol: protocol}
}

func sendEvent(ctx context.Context, output chan<- LMEvent, event LMEvent) bool {
	select {
	case output <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func streamErrorEvent(protocol string, err error) LMEvent {
	message := "stream failed"
	if err != nil {
		message = err.Error()
	}
	body := map[string]any{"type": "error", "error": map[string]any{"message": message}}
	if protocol == "openai" {
		return OpenAIChatEvent(body)
	}
	return OpenAIResponsesEvent(body)
}

func EventName(event LMEvent) string {
	responses, ok := event.(OpenAIResponsesEvent)
	if !ok {
		return ""
	}
	if name, ok := responses[eventNameKey].(string); ok {
		return name
	}
	name, _ := responses["type"].(string)
	return name
}

func EventData(event LMEvent) ([]byte, error) {
	raw, ok := MapEvent(event)
	if !ok {
		return nil, fmt.Errorf("unknown event type %T", event)
	}
	copy := cloneMap(raw)
	delete(copy, eventNameKey)
	return json.Marshal(copy)
}

func MapEvent(event LMEvent) (map[string]any, bool) {
	switch event := event.(type) {
	case OpenAIChatEvent:
		return map[string]any(event), true
	case OpenAIResponsesEvent:
		return map[string]any(event), true
	default:
		return nil, false
	}
}

func (r *StreamResponse) Get(path string) (any, bool) {
	switch path {
	case "event":
		if r.events == nil {
			return nil, false
		}
		return r.events, true
	case "context":
		return r.ctx, true
	default:
		return r.document.Get(path)
	}
}

func (r *StreamResponse) Set(path string, value any) error {
	if path == "event" || path == "context" {
		return fmt.Errorf("response path %q is read-only", path)
	}
	return r.document.Set(path, value)
}

func (r *StreamResponse) SetDefault(path string, value any) error {
	if path == "event" || path == "context" {
		return fmt.Errorf("response path %q is read-only", path)
	}
	return applyDefault(r, path, value)
}

func (r *StreamResponse) Delete(path string) error {
	if path == "event" || path == "context" {
		return fmt.Errorf("response path %q is read-only", path)
	}
	return r.document.Delete(path)
}

func (r *StreamResponse) ConvertFrom(src LMDocument) (LMDocument, error) {
	source, ok := src.(LMResponse)
	if !ok {
		return r.document.ConvertFrom(src)
	}
	sourceType, ok := TypeOf(source)
	if !ok {
		return nil, errors.New("unknown source response document type")
	}
	if sourceType == r.protocol {
		return source, nil
	}
	value, ok := source.Get("event")
	if !ok {
		return nil, errors.New("source response has no event channel")
	}
	events, ok := eventChannel(value)
	if !ok {
		return nil, errors.New("source response event path has invalid type")
	}
	ctx := r.ctx
	if value, ok := source.Get("context"); ok {
		if sourceContext, ok := value.(context.Context); ok && sourceContext != nil {
			ctx = sourceContext
		}
	}
	return adaptResponseEvents(r.document, sourceType, r.protocol, events, ctx)
}

func eventChannel(value any) (<-chan LMEvent, bool) {
	switch channel := value.(type) {
	case <-chan LMEvent:
		return channel, true
	case chan LMEvent:
		return channel, true
	default:
		return nil, false
	}
}
