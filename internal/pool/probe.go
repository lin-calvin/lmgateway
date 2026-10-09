package pool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

// ProbeTag / ProbeBackendTag mark synthetic probe traffic in request metadata;
// the spend tracker flattens them into TS tags (meta_probe / meta_pool_backend).
const (
	ProbeTag        = "probe"
	ProbeBackendTag = "pool_backend"
)

// DefaultProbeMaxTokens is the probe output length when a model does not override it.
const DefaultProbeMaxTokens = 1024

// Probe dispatches one synthetic streaming request to a backend model id. The
// request is served at the http source (skipping observe/ratelimit) so it never
// recurses through the pool and never trips the rotation reporter. Measurement
// itself is done by the spend tracker (ttft_ms/generation_ms land in TS).
//
// The prompt embeds a fresh timestamp so every probe misses the upstream prefix
// cache, giving a comparable cold measurement.
func Probe(dispatcher func() *dispatch.Dispatcher, ctrl *Controller, poolName, backend string, maxTokens int) {
	d := dispatcher()
	if d == nil {
		return
	}
	if maxTokens <= 0 {
		maxTokens = DefaultProbeMaxTokens
	}
	doc, err := lm.NewRequest("openai", map[string]any{
		"model":      backend,
		"stream":     true,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{"role": "user", "content": probePrompt()},
		},
		"metadata": map[string]any{
			ProbeTag:        true,
			ProbeBackendTag: backend,
		},
	})
	if err != nil {
		return
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	pkt.Set(packet.KeyStart, time.Now())
	pkt.Set(packet.KeyReply, &discardReply{})

	out := d.Serve(pkt, packet.SourceHTTP)
	if class, ok := out.ErrorClass(); ok && class == packet.ClassRateLimit {
		retry, _ := out.ErrorRetryAfter()
		ctrl.Report(poolName, backend, class, retry)
	}
}

// probePrompt returns a fixed ~1024-token filler prompt with a unique timestamp
// suffix so the upstream prefix cache never hits.
func probePrompt() string {
	const sentence = "The quick brown fox jumps over the lazy dog while the gateway routes a request to a backend and records its latency. "
	unit := strings.Repeat(sentence, 4)
	return fmt.Sprintf("%s\n\nCurrent timestamp: %d", strings.Repeat(unit, 9), time.Now().UnixNano())
}

// discardReply drains an event stream purely to timestamp first output and end,
// mirroring the HTTP stream reply so the spend tracker can derive ttft/generation.
type discardReply struct {
	committed bool
}

func (r *discardReply) Committed() bool { return r.committed }

func (r *discardReply) WriteStream(pkt packet.Packet) packet.Packet {
	response, ok := pkt.Response()
	if !ok {
		return pkt.Fail(packet.ErrInternal, "missing response document")
	}
	value, ok := response.Get("event")
	if !ok {
		return pkt.Fail(packet.ErrInternal, "response has no event stream")
	}
	events, ok := value.(<-chan lm.LMEvent)
	if !ok {
		return pkt.Fail(packet.ErrInternal, "response event stream has invalid type")
	}
	r.committed = true
	for event := range events {
		if _, ok := pkt[packet.KeyFirst]; !ok && probeHasOutput(event) {
			pkt.Set(packet.KeyFirst, time.Now())
		}
	}
	pkt.Set(packet.KeyEnd, time.Now())
	return pkt
}

// probeHasOutput mirrors httpapi.hasOutput: any content, reasoning or tool-call
// delta counts as the first output.
func probeHasOutput(event lm.LMEvent) bool {
	raw, ok := lm.MapEvent(event)
	if !ok {
		return false
	}
	if name := lm.EventName(event); name != "" {
		switch name {
		case "response.output_text.delta", "response.reasoning_text.delta",
			"response.reasoning_summary.delta", "response.reasoning_summary_text.delta",
			"response.function_call_arguments.delta":
			return probeEventString(raw["delta"]) != ""
		}
	}
	choices, _ := raw["choices"].([]any)
	for _, value := range choices {
		choice, _ := value.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if probeEventString(delta["content"]) != "" || probeEventString(delta["reasoning_content"]) != "" {
			return true
		}
		for _, rawCall := range asSliceAny(delta["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if probeEventString(fn["name"]) != "" || probeEventString(fn["arguments"]) != "" {
				return true
			}
		}
	}
	return false
}

func probeEventString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func asSliceAny(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}
