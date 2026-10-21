package handler

import (
	"context"
	"log"
	"time"

	"lmgateway/internal/hook"
	"lmgateway/internal/keytag"
	"lmgateway/internal/store"
	"lmgateway/internal/usage"
)

// Pricing 每百万 token 单价（USD），按 provider 配置
type Pricing struct {
	InputPerMtok  float64
	OutputPerMtok float64
}

// SpendSink is a usage.collector subscriber that writes one spend record per
// request to the time-series store (and optionally logs). It never blocks the
// request: the store append is buffered.
type SpendSink struct {
	ts      store.TSStore
	pricing func(model string) (Pricing, bool)
}

// NewSpendSink builds a sink. ts may be nil (log only); pricing may be nil.
func NewSpendSink(ts store.TSStore, pricing func(model string) (Pricing, bool)) *SpendSink {
	return &SpendSink{ts: ts, pricing: pricing}
}

// Record handles one usage event.
func (s *SpendSink) Record(u usage.Usage) {
	stream := "false"
	if u.Stream {
		stream = "true"
	}
	tag := u.KeyTag
	if tag == "" {
		tag = "-"
	}
	log.Printf("[usage] source=%s model=%s prompt=%d completion=%d total=%d reasoning=%d cached=%d stream=%s tag=%s",
		u.Provider, u.Model, u.Prompt, u.Completion, u.Total, u.Reasoning, u.Cached, stream, tag)
	if s.ts == nil {
		return
	}

	rec := &store.TSRecord{
		Ts:     time.Now().UTC(),
		Stream: "spend",
		Tags: map[string]string{
			"provider": u.Provider,
			"model":    u.Model,
			"stream":   stream,
			"status":   "ok",
		},
		Fields: map[string]any{
			"prompt_tokens":     float64(u.Prompt),
			"completion_tokens": float64(u.Completion),
			"total_tokens":      float64(u.Total),
			"reasoning_tokens":  float64(u.Reasoning),
			"cached_tokens":     float64(u.Cached),
		},
	}
	// 请求元数据平铺成 tag（litellm 风格）：metadata:{...} → meta_<key>
	for k, v := range u.Metadata {
		rec.Tags["meta_"+k] = v
	}
	if u.KeyTag != "" {
		rec.Tags[keytag.TagField] = u.KeyTag
	}
	// 成本按 model 粒度（pricing 由 model 实体提供）
	if s.pricing != nil {
		if pr, ok := s.pricing(u.Model); ok {
			rec.Fields["cost"] = float64(u.Prompt)/1e6*pr.InputPerMtok + float64(u.Completion)/1e6*pr.OutputPerMtok
		}
	}
	if u.HasStart {
		rec.Fields["latency_ms"] = u.LatencyMs
	}
	if u.HasTTFT {
		rec.Fields["ttft_ms"] = u.TTFTMs
		rec.Fields["generation_ms"] = u.GenerationMs
		rec.Fields["generation_tokens"] = u.GenerationTokens
		rec.Fields["ttft_samples"] = float64(1)
		rec.Fields["generation_samples"] = float64(1)
	}
	_ = s.ts.Append(context.Background(), rec)
}

// NewSpendRecorder returns a "usage" collector wired to a single spend sink.
// Convenience for tests and for builds that don't provide a shared registry.
func NewSpendRecorder(ts store.TSStore, pricing map[string]Pricing) UsageCollector {
	hooks := hook.New[usage.Usage]()
	hooks.Register(NewSpendSink(ts, func(model string) (Pricing, bool) {
		p, ok := pricing[model]
		return p, ok
	}).Record)
	return UsageCollector{Hooks: hooks}
}
