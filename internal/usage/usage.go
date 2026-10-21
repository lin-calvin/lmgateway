// Package usage extracts request/response performance data — token counts and
// timing — from a dispatch packet. It is the single place that normalizes the
// many provider-specific usage fields, so the spend recorder, the OpenTelemetry
// observer, the logprobs exporter and the pool selector all read the same shape.
package usage

import (
	"strconv"
	"time"

	"lmgateway/internal/keytag"
	"lmgateway/internal/packet"
)

// Counts is the normalized token usage of one response.
type Counts struct {
	Prompt     int
	Completion int
	Total      int
	Reasoning  int
	Cached     int

	HasPrompt     bool
	HasCompletion bool
	HasTotal      bool
}

// Usage is the full performance record for one request/response.
type Usage struct {
	Model    string
	Provider string
	Stream   bool
	// KeyTag is the client-declared attribution label (key suffix or body
	// key_tag). It never affects authorization.
	KeyTag string
	Counts

	// Metadata is the request's scalar metadata, flattened from req.metadata
	// (nested objects/arrays are dropped).
	Metadata map[string]string

	LatencyMs        float64
	TTFTMs           float64
	GenerationMs     float64
	GenerationTokens float64
	HasStart         bool
	HasTTFT          bool
}

// Extract reads usage and timing from a packet's response, if present.
func Extract(pkt packet.Packet) (Usage, bool) {
	resp, ok := pkt.Map(packet.KeyResp)
	if !ok {
		return Usage{}, false
	}
	u := Usage{Counts: CountsFromMap(usageMap(resp))}
	if model, ok := resp["model"].(string); ok {
		u.Model = model
	}
	u.Provider, _ = pkt.Str(packet.KeyProvider)
	if u.Provider == "" {
		// Legacy packets may still carry source.
		u.Provider, _ = pkt.Str("source")
	}
	if req, ok := pkt.Map(packet.KeyReq); ok {
		u.Stream, _ = req["stream"].(bool)
		if tag, ok := req[keytag.TagField].(string); ok && keytag.ValidTag(tag) {
			u.KeyTag = tag
		}
		if md, ok := req["metadata"].(map[string]any); ok {
			u.Metadata = make(map[string]string, len(md))
			for k, v := range md {
				if sv, ok := scalarString(v); ok {
					u.Metadata[k] = sv
				}
			}
		}
	}

	start, ok := pkt[packet.KeyStart].(time.Time)
	if !ok {
		return u, true
	}
	u.HasStart = true
	end := time.Now()
	if v, ok := pkt[packet.KeyEnd].(time.Time); ok {
		end = v
	}
	u.LatencyMs = Milliseconds(end.Sub(start))
	if first, ok := pkt[packet.KeyFirst].(time.Time); ok && !first.Before(start) && !end.Before(first) {
		u.HasTTFT = true
		u.TTFTMs = Milliseconds(first.Sub(start))
		u.GenerationMs = Milliseconds(end.Sub(first))
		u.GenerationTokens = float64(u.Counts.Completion)
	}
	return u, true
}

// CountsFromMap normalizes the many provider-specific usage shapes into Counts.
func CountsFromMap(usage map[string]any) Counts {
	var c Counts
	// Prefer the chat key, fall back to the responses key (also when the
	// primary is present but zero, matching the historical recorder).
	if v, ok := number(usage, "prompt_tokens"); ok && v != 0 {
		c.Prompt, c.HasPrompt = v, true
	} else if v, ok := number(usage, "input_tokens"); ok {
		c.Prompt, c.HasPrompt = v, true
	} else if v, ok := number(usage, "prompt_tokens"); ok {
		c.Prompt, c.HasPrompt = v, true
	}
	if v, ok := number(usage, "completion_tokens"); ok && v != 0 {
		c.Completion, c.HasCompletion = v, true
	} else if v, ok := number(usage, "output_tokens"); ok {
		c.Completion, c.HasCompletion = v, true
	} else if v, ok := number(usage, "completion_tokens"); ok {
		c.Completion, c.HasCompletion = v, true
	}
	if v, ok := number(usage, "total_tokens"); ok {
		c.Total, c.HasTotal = v, true
	}
	if v, ok := number(Nested(usage, "completion_tokens_details"), "reasoning_tokens"); ok {
		c.Reasoning = v
	} else if v, ok := number(Nested(usage, "output_tokens_details"), "reasoning_tokens"); ok {
		c.Reasoning = v
	}
	c.Cached = CacheHitTokens(usage)
	return c
}

// FieldInt reads an integer field, tolerating the JSON number types.
func FieldInt(m map[string]any, key string) int {
	v, _ := number(m, key)
	return v
}

// Nested returns a nested object field, if present.
func Nested(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// CacheHitTokens normalizes the prompt-cache hit field across providers.
func CacheHitTokens(usage map[string]any) int {
	if n := FieldInt(Nested(usage, "prompt_tokens_details"), "cached_tokens"); n > 0 {
		return n
	}
	for _, key := range []string{
		"prompt_cache_hit_tokens", // DeepSeek
		"cache_read_input_tokens", // some OpenAI-compatible providers
		"cache_read_tokens",
	} {
		if n := FieldInt(usage, key); n > 0 {
			return n
		}
	}
	if n := FieldInt(Nested(usage, "input_tokens_details"), "cached_tokens"); n > 0 {
		return n
	}
	return 0
}

// Milliseconds converts a duration to float milliseconds.
func Milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// scalarString flattens a simple scalar metadata value to a string; complex
// objects/arrays are ignored.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	}
	return "", false
}

func usageMap(resp map[string]any) map[string]any {
	if v, ok := resp["usage"].(map[string]any); ok {
		return v
	}
	return nil
}

func number(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}
