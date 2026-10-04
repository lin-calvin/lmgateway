package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

const maxObservationValue = 16384

type Observe struct{}

func (Observe) Handle(pkt packet.Packet, serve dispatch.Serve) (out packet.Packet) {
	ctx, _ := pkt[packet.KeyCtx].(context.Context)
	if ctx == nil {
		ctx = context.Background()
	}
	request, _ := pkt.Request()
	ctx, span := otel.Tracer("lmgateway/observe").Start(ctx, "gen_ai.chat")
	pkt.Set(packet.KeyCtx, ctx)
	if request != nil {
		setRequestAttributes(span, request)
	}
	defer func() {
		setResponseAttributes(span, out)
		if kind, ok := out.ErrorKind(); ok {
			kind = safeObservationString(kind)
			span.SetAttributes(attribute.String("error.type", kind))
			span.SetStatus(codes.Error, kind)
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}()
	return serve(pkt)
}

func setRequestAttributes(span trace.Span, request lm.LMDocument) {
	if value, found := request.Get("model"); found {
		if model, ok := value.(string); ok && model != "" {
			span.SetAttributes(
				attribute.String("gen_ai.request.model", safeObservationString(model)),
				attribute.String("llm.model_name", safeObservationString(model)),
			)
		}
	}
	if value, found := request.Get("stream"); found {
		if streaming, ok := value.(bool); ok {
			span.SetAttributes(
				attribute.Bool("gen_ai.request.stream", streaming),
				attribute.String("llm.is_streaming", fmt.Sprint(streaming)),
			)
		}
	}
	if typ, ok := lm.TypeOf(request); ok {
		span.SetAttributes(attribute.String("lmgateway.request.type", safeObservationString(typ)))
	}
	if raw, ok := lm.Map(request); ok {
		if messages, found := raw["messages"]; found {
			setMessageAttributes(span, "llm.input_messages", messages)
			setInputValue(span, messages)
		}
		if input, found := raw["input"]; found {
			setInputValue(span, input)
		}
		if instructions, found := raw["instructions"]; found {
			setInputValue(span, instructions)
		}
	}
}

func setResponseAttributes(span trace.Span, pkt packet.Packet) {
	provider, _ := pkt.Str(packet.KeyProvider)
	response, ok := pkt.Response()
	if !ok {
		return
	}
	raw, ok := lm.Map(response)
	if !ok {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	if provider != "" {
		attrs = append(attrs,
			attribute.String("gen_ai.provider.name", safeObservationString(provider)),
			attribute.String("llm.provider", safeObservationString(provider)),
			attribute.String("llm.system", safeObservationString(provider)),
		)
	}
	if model, ok := raw["model"].(string); ok && model != "" {
		attrs = append(attrs,
			attribute.String("gen_ai.response.model", safeObservationString(model)),
			attribute.String("llm.model_name", safeObservationString(model)),
		)
	}
	if id, ok := raw["id"].(string); ok && id != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", safeObservationString(id)))
	}
	if finish := responseFinish(raw); finish != "" {
		attrs = append(attrs,
			attribute.String("llm.finish_reason", safeObservationString(finish)),
			attribute.StringSlice("gen_ai.response.finish_reasons", []string{safeObservationString(finish)}),
		)
	}
	span.SetAttributes(attrs...)
	setUsageAttributes(span, raw["usage"])
	setResponseContentAttributes(span, raw)
}

func setUsageAttributes(span trace.Span, value any) {
	usage, ok := value.(map[string]any)
	if !ok {
		return
	}
	input, inputKnown := usageNumber(usage, "prompt_tokens")
	if !inputKnown {
		input, inputKnown = usageNumber(usage, "input_tokens")
	}
	output, outputKnown := usageNumber(usage, "completion_tokens")
	if !outputKnown {
		output, outputKnown = usageNumber(usage, "output_tokens")
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	if inputKnown {
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.input_tokens", input),
			attribute.Int("llm.token_count.prompt", input),
		)
	}
	if outputKnown {
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.output_tokens", output),
			attribute.Int("llm.token_count.completion", output),
		)
	}
	if total, known := usageNumber(usage, "total_tokens"); known {
		attrs = append(attrs, attribute.Int("llm.token_count.total", total))
	}
	if cached := cacheHitTokens(usage); cached > 0 {
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.cache_read.input_tokens", cached),
			attribute.Int("llm.token_count.prompt_details.cache_read", cached),
		)
	}
	if reasoning := intField(nested(usage, "completion_tokens_details"), "reasoning_tokens"); reasoning > 0 {
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.reasoning.output_tokens", reasoning),
			attribute.Int("llm.token_count.completion_details.reasoning", reasoning),
		)
	}
	span.SetAttributes(attrs...)
}

func setResponseContentAttributes(span trace.Span, raw map[string]any) {
	if choices, ok := raw["choices"].([]any); ok {
		for index, value := range choices {
			choice, _ := value.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			setMessageAttributes(span, fmt.Sprintf("llm.output_messages.%d", index), []any{message})
			if content := messageText(message["content"]); content != "" {
				span.SetAttributes(
					attribute.String("output.value", limitObservationValue(content)),
					attribute.String("output.mime_type", "text/plain"),
				)
			}
		}
		return
	}
	if output, ok := raw["output"].([]any); ok {
		for index, value := range output {
			item, _ := value.(map[string]any)
			setMessageAttributes(span, fmt.Sprintf("llm.output_messages.%d", index), []any{item})
			if content := messageText(item["content"]); content != "" {
				span.SetAttributes(
					attribute.String("output.value", limitObservationValue(content)),
					attribute.String("output.mime_type", "text/plain"),
				)
			}
		}
	}
}

func setMessageAttributes(span trace.Span, prefix string, value any) {
	messages, ok := value.([]any)
	if !ok {
		return
	}
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		if role, ok := message["role"].(string); ok {
			span.SetAttributes(attribute.String(fmt.Sprintf("%s.%d.message.role", prefix, index), safeObservationString(role)))
		}
		if content := messageText(message["content"]); content != "" {
			span.SetAttributes(attribute.String(fmt.Sprintf("%s.%d.message.content", prefix, index), limitObservationValue(content)))
		}
	}
}

func setInputValue(span trace.Span, value any) {
	text := messageText(value)
	if text == "" {
		encoded, err := json.Marshal(value)
		if err != nil {
			return
		}
		text = string(encoded)
	}
	span.SetAttributes(
		attribute.String("input.value", limitObservationValue(text)),
		attribute.String("input.mime_type", "text/plain"),
	)
}

func messageText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
				if text, ok := part["output_text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		if text, ok := value["text"].(string); ok {
			return text
		}
		if content, ok := value["content"]; ok {
			return messageText(content)
		}
	}
	return ""
}

func responseFinish(raw map[string]any) string {
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			finish, _ := choice["finish_reason"].(string)
			return finish
		}
	}
	if details, ok := raw["incomplete_details"].(map[string]any); ok {
		reason, _ := details["reason"].(string)
		return reason
	}
	if status, ok := raw["status"].(string); ok {
		return status
	}
	return ""
}

func usageNumber(values map[string]any, key string) (int, bool) {
	value, ok := values[key]
	if !ok || value == nil {
		return 0, false
	}
	switch value := value.(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case int64:
		return int(value), true
	}
	return 0, false
}

func safeObservationString(value string) string {
	return strings.ToValidUTF8(value, "\uFFFD")
}

func limitObservationValue(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= maxObservationValue {
		return value
	}
	return strings.ToValidUTF8(value[:maxObservationValue], "\uFFFD")
}
