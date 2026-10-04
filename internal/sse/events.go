package sse

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"lmgateway/internal/lm"
)

func ChatEvents(ctx context.Context, body io.ReadCloser, model string) <-chan lm.LMEvent {
	return eventStream(ctx, body, model, false)
}

func ResponsesEvents(ctx context.Context, body io.ReadCloser, model string) <-chan lm.LMEvent {
	return eventStream(ctx, body, model, true)
}

func eventStream(ctx context.Context, body io.ReadCloser, model string, responses bool) <-chan lm.LMEvent {
	if ctx == nil {
		ctx = context.Background()
	}
	output := make(chan lm.LMEvent)
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = body.Close() }) }
	if ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			closeBody()
		}()
	}
	go func() {
		defer close(output)
		defer closeBody()
		stream := New(body)
		if responses {
			stream = NewNativeResponses(body)
		}
		for {
			if responses {
				event, err := stream.NextEvent()
				if err == io.EOF {
					return
				}
				if err != nil {
					sendEvent(ctx, output, responseErrorEvent(err))
					return
				}
				var raw map[string]any
				if err := json.Unmarshal(event.Data, &raw); err != nil {
					sendEvent(ctx, output, responseErrorEvent(err))
					return
				}
				if model != "" {
					if response, ok := raw["response"].(map[string]any); ok {
						response["model"] = model
					}
					if _, ok := raw["model"]; ok {
						raw["model"] = model
					}
				}
				if !sendEvent(ctx, output, lm.NewResponsesEvent(raw, event.Name)) {
					return
				}
				continue
			}
			chunk, err := stream.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				sendEvent(ctx, output, chatErrorEvent(err))
				return
			}
			if model != "" {
				chunk["model"] = model
			}
			if !sendEvent(ctx, output, lm.NewChatEvent(chunk)) {
				return
			}
		}
	}()
	return output
}

func sendEvent(ctx context.Context, output chan<- lm.LMEvent, event lm.LMEvent) bool {
	select {
	case output <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func chatErrorEvent(err error) lm.LMEvent {
	return lm.NewChatEvent(map[string]any{"type": "error", "error": map[string]any{"message": err.Error()}})
}

func responseErrorEvent(err error) lm.LMEvent {
	return lm.NewResponsesEvent(map[string]any{"type": "error", "error": map[string]any{"message": err.Error()}}, "error")
}
