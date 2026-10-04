package handler

import (
	"context"
	"log"
	"strconv"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/store"
)

// Pricing 每百万 token 单价（USD），按 provider 配置
type Pricing struct {
	InputPerMtok  float64
	OutputPerMtok float64
}

// SpendRecorder 响应相位 handler：从 resp 提取 usage/延迟，算 cost，入 TS 存储（非阻塞）。
// ts 为 nil 时只打日志（无持久化环境）。包原样透传。
type SpendRecorder struct {
	ts      store.TSStore
	pricing map[string]Pricing
	log     bool
}

func NewSpendRecorder(ts store.TSStore, pricing map[string]Pricing) *SpendRecorder {
	return &SpendRecorder{ts: ts, pricing: pricing, log: true}
}

// NewSpendLogger 仅日志的版本（兼容旧路径）
func NewSpendLogger() *SpendRecorder {
	return &SpendRecorder{ts: nil, pricing: nil, log: true}
}

func (s *SpendRecorder) Handle(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
	next := dispatch.Serve(func(out packet.Packet, sources ...string) packet.Packet {
		if len(serves) == 0 {
			return out
		}
		return serves[0](out, sources...)
	})
	resp, ok := pkt.Map("resp")
	if !ok {
		return next(pkt)
	}
	usage, _ := resp["usage"].(map[string]any)
	provider, _ := pkt.Str("provider")
	if provider == "" {
		// Legacy packets may still carry source; routing no longer depends on it.
		provider, _ = pkt.Str("source")
	}
	model, _ := resp["model"].(string)

	pt := intField(usage, "prompt_tokens")
	if pt == 0 {
		pt = intField(usage, "input_tokens")
	}
	ct := intField(usage, "completion_tokens")
	if ct == 0 {
		ct = intField(usage, "output_tokens")
	}
	tt := intField(usage, "total_tokens")
	rt := intField(nested(usage, "completion_tokens_details"), "reasoning_tokens")
	if rt == 0 {
		rt = intField(nested(usage, "output_tokens_details"), "reasoning_tokens")
	}
	cached := cacheHitTokens(usage)
	if cached == 0 {
		cached = intField(nested(usage, "input_tokens_details"), "cached_tokens")
	}

	req, _ := pkt.Map("req")
	stream := "false"
	if s, _ := req["stream"].(bool); s {
		stream = "true"
	}

	if s.log {
		log.Printf("[usage] source=%s model=%s prompt=%d completion=%d total=%d reasoning=%d cached=%d stream=%s",
			provider, model, pt, ct, tt, rt, cached, stream)
	}
	if s.ts == nil {
		return next(pkt)
	}

	recordedAt := time.Now()
	rec := &store.TSRecord{
		Ts:     recordedAt.UTC(),
		Stream: "spend",
		Tags: map[string]string{
			"provider": provider,
			"model":    model,
			"stream":   stream,
			"status":   "ok",
		},
		Fields: map[string]any{
			"prompt_tokens":     float64(pt),
			"completion_tokens": float64(ct),
			"total_tokens":      float64(tt),
			"reasoning_tokens":  float64(rt),
			"cached_tokens":     float64(cached),
		},
	}
	// 请求元数据平铺成 tag（litellm 风格）：metadata:{...} → meta_<key>，任意口径可查
	if md, ok := req["metadata"].(map[string]any); ok {
		for k, v := range md {
			if sv, ok := scalarString(v); ok {
				rec.Tags["meta_"+k] = sv
			}
		}
	}
	// 成本按 model 粒度（pricing 由 model 实体提供）
	if pr, ok := s.pricing[model]; ok {
		rec.Fields["cost"] = float64(pt)/1e6*pr.InputPerMtok + float64(ct)/1e6*pr.OutputPerMtok
	}
	if st, ok := pkt[packet.KeyStart].(time.Time); ok {
		end := recordedAt
		if value, ok := pkt[packet.KeyEnd].(time.Time); ok {
			end = value
		}
		rec.Fields["latency_ms"] = milliseconds(end.Sub(st))
		if first, ok := pkt[packet.KeyFirst].(time.Time); ok && !first.Before(st) && !end.Before(first) {
			rec.Fields["ttft_ms"] = milliseconds(first.Sub(st))
			rec.Fields["generation_ms"] = milliseconds(end.Sub(first))
			rec.Fields["generation_tokens"] = float64(ct)
			rec.Fields["ttft_samples"] = float64(1)
			rec.Fields["generation_samples"] = float64(1)
		}
	}
	_ = s.ts.Append(context.Background(), rec)
	return next(pkt)
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// scalarString 把 metadata 的简单标量转成 tag 字符串；复杂对象忽略
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

// Usage 仅日志的响应相位 handler（无持久化时注册）
func Usage(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
	return NewSpendLogger().Handle(pkt, serves...)
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func nested(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// cacheHitTokens 归一化不同 provider 的 prompt cache hit 字段。
// 当前只记录命中量，不计算命中率/未命中量。
func cacheHitTokens(usage map[string]any) int {
	if n := intField(nested(usage, "prompt_tokens_details"), "cached_tokens"); n > 0 {
		return n
	}
	for _, key := range []string{
		"prompt_cache_hit_tokens", // DeepSeek
		"cache_read_input_tokens", // 部分 OpenAI-compatible provider
		"cache_read_tokens",
	} {
		if n := intField(usage, key); n > 0 {
			return n
		}
	}
	return 0
}
