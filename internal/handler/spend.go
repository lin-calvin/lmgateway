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

// Pricing 每百万 token 单价（USD），按 model 实体配置。
//
// 注意 prompt_tokens 是**包含** cached_tokens 的（OpenAI 语义），所以计费时必须把
// 命中缓存的那部分拆出来单独计价，否则缓存命中会被按全价收费——这正是缓存折扣存在的意义。
type Pricing struct {
	InputPerMtok       float64
	OutputPerMtok      float64
	CachedInputPerMtok float64
	// CachedInputFree 缓存读取免费（显式开关）。
	// 与 CachedInputPerMtok 二选一：0 表示"未配置"，不能用 0 兼任"免费"。
	CachedInputFree bool
}

// CachedInputPrice 缓存命中的输入单价。
//
// 优先级：显式免费 → 显式缓存价 → 回退普通输入价（未配置时保守，不少算）。
func (p Pricing) CachedInputPrice() float64 {
	if p.CachedInputFree {
		return 0
	}
	if p.CachedInputPerMtok > 0 {
		return p.CachedInputPerMtok
	}
	return p.InputPerMtok
}

// Cost 单次调用的实际成本。
// cached 会被夹到 [0, prompt]：上游 usage 偶有不一致时不能让成本变成负数。
func (p Pricing) Cost(promptTokens, cachedTokens, completionTokens int) float64 {
	cached := cachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > promptTokens {
		cached = promptTokens
	}
	fresh := promptTokens - cached
	return float64(fresh)/1e6*p.InputPerMtok +
		float64(cached)/1e6*p.CachedInputPrice() +
		float64(completionTokens)/1e6*p.OutputPerMtok
}

// CostUpperBound 预留（预扣）用的成本上界。
// 请求入口还不知道会命中多少缓存，而缓存价有可能**高于**普通输入价（少见但合法），
// 所以取两者较大者作为输入单价，保证预留不低于实际。
func (p Pricing) CostUpperBound(promptTokens, completionTokens int) float64 {
	unit := p.InputPerMtok
	if cached := p.CachedInputPrice(); cached > unit {
		unit = cached
	}
	return float64(promptTokens)/1e6*unit + float64(completionTokens)/1e6*p.OutputPerMtok
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
	// 租户归属:数据面身份由鉴权中间件注入 packet。这三个 tag 是配额校准
	// (tenancy.Calibrate)与按租户分账的依据。
	if identity, ok := IdentityOf(pkt); ok {
		if identity.TenantID != "" {
			rec.Tags["tenant"] = identity.TenantID
		}
		if identity.ProjectID != "" {
			rec.Tags["project"] = identity.ProjectID
		}
		if identity.KeyID != "" {
			rec.Tags["key"] = identity.KeyID
			rec.Tags["key_prefix"] = identity.KeyPrefix
		}
	}
	// 请求元数据平铺成 tag（litellm 风格）：metadata:{...} → meta_<key>，任意口径可查
	if md, ok := req["metadata"].(map[string]any); ok {
		for k, v := range md {
			if sv, ok := scalarString(v); ok {
				rec.Tags["meta_"+k] = sv
			}
		}
	}
	// 成本按 model 粒度（pricing 由 model 实体提供）；命中缓存的部分走缓存价
	if pr, ok := s.pricing[model]; ok {
		rec.Fields["cost"] = pr.Cost(pt, cached, ct)
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
