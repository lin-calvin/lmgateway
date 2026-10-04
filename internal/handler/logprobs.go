package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

type LogprobsConfig struct {
	WebhookURL string
	TimeoutSec int
}

type LogprobsExporter struct {
	webhookURL string
	client     *http.Client
	inFlight   chan struct{}
}

func NewLogprobsExporter(cfg LogprobsConfig) *LogprobsExporter {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	exporter := &LogprobsExporter{
		webhookURL: cfg.WebhookURL,
		client:     &http.Client{Timeout: timeout},
		inFlight:   make(chan struct{}, 256),
	}
	return exporter
}

func (e *LogprobsExporter) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	if e.webhookURL == "" {
		return serve(pkt)
	}
	payload, ok := buildLogprobsPayload(pkt)
	if !ok {
		return serve(pkt)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return serve(pkt)
	}
	select {
	case e.inFlight <- struct{}{}:
		go e.export(body)
	default:
	}
	return serve(pkt)
}

func (e *LogprobsExporter) export(body []byte) {
	defer func() { <-e.inFlight }()
	request, err := http.NewRequest(http.MethodPost, e.webhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

func buildLogprobsPayload(pkt packet.Packet) (map[string]any, bool) {
	response, ok := pkt.Map(packet.KeyResp)
	if !ok {
		return nil, false
	}
	document, _ := pkt.Response()
	documentType, _ := lm.TypeOf(document)
	logprobs, ok := extractLogprobs(response, documentType)
	if !ok {
		return nil, false
	}

	payload := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"logprobs":  logprobs,
		"response":  responsePayload(response),
	}
	if documentType != "" {
		payload["doc_type"] = documentType
	}
	if provider, ok := pkt.Str(packet.KeyProvider); ok && provider != "" {
		payload["provider"] = provider
	}
	if model, ok := response["model"].(string); ok && model != "" {
		payload["model"] = model
	}
	if id, ok := response["id"].(string); ok && id != "" {
		payload["response_id"] = id
	}
	if usage, ok := response["usage"].(map[string]any); ok && len(usage) > 0 {
		payload["usage"] = usage
	}
	if finish := responseFinish(response); finish != "" {
		payload["finish_reason"] = finish
	}
	if request, ok := pkt.Request(); ok {
		if streaming, ok := request.Get("stream"); ok {
			payload["stream"] = streaming == true
		}
	}
	if metadata, ok := pkt[packet.KeyHTTPMeta].(map[string]any); ok {
		if requestID, ok := metadata["request_id"].(string); ok && requestID != "" {
			payload["request_id"] = requestID
		}
		if sessionID, ok := metadata["session_id"].(string); ok && sessionID != "" {
			payload["session_id"] = sessionID
		}
	}
	return payload, true
}

func responsePayload(response map[string]any) map[string]any {
	payload := map[string]any{}
	if choices, ok := response["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			if content, ok := message["content"]; ok {
				payload["content"] = content
			}
			if reasoning, ok := message["reasoning_content"]; ok {
				payload["reasoning"] = reasoning
			}
			if calls, ok := message["tool_calls"]; ok {
				payload["tool_calls"] = calls
			}
			break
		}
	}
	if output, ok := response["output"].([]any); ok {
		payload["output"] = output
	}
	return payload
}

func extractLogprobs(response map[string]any, documentType string) (map[string]any, bool) {
	if documentType == "openai_response" || (documentType == "" && response["output"] != nil) {
		items := responsesLogprobs(response)
		if len(items) == 0 {
			return nil, false
		}
		return map[string]any{"format": "responses", "items": items}, true
	}
	choices := chatLogprobs(response)
	if len(choices) == 0 {
		return nil, false
	}
	return map[string]any{"format": "chat", "choices": choices}, true
}

func chatLogprobs(response map[string]any) []any {
	choices, _ := response["choices"].([]any)
	out := make([]any, 0, len(choices))
	for index, raw := range choices {
		choice, _ := raw.(map[string]any)
		logprobs, _ := choice["logprobs"].(map[string]any)
		if len(logprobs) == 0 {
			continue
		}
		item := map[string]any{"index": index}
		for _, key := range []string{"content", "refusal", "reasoning_content"} {
			if entries, ok := logprobs[key].([]any); ok && len(entries) > 0 {
				item[key] = entries
			}
		}
		if len(item) > 1 {
			out = append(out, item)
		}
	}
	return out
}

func responsesLogprobs(response map[string]any) []any {
	output, _ := response["output"].([]any)
	out := []any{}
	for outputIndex, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		for contentIndex, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			entries, _ := part["logprobs"].([]any)
			if len(entries) == 0 {
				continue
			}
			entry := map[string]any{
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"logprobs":      entries,
			}
			if id, ok := item["id"].(string); ok && id != "" {
				entry["item_id"] = id
			}
			out = append(out, entry)
		}
	}
	return out
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}
