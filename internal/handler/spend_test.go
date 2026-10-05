package handler

import (
	"context"
	"testing"
	"time"

	"lmgateway/internal/packet"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestSpendRecorderEnrichment(t *testing.T) {
	back := mem.NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	s := NewSpendRecorder(ts, map[string]Pricing{"m1": {InputPerMtok: 1.0, OutputPerMtok: 2.0}})

	start := time.Now().Add(-time.Second)
	pkt := packet.NewReq(map[string]any{
		"model":    "m1",
		"stream":   true,
		"metadata": map[string]any{"team": "core", "n": 3, "nested": map[string]any{"x": 1}},
	})
	pkt.Set("start", start)
	first := start.Add(250 * time.Millisecond)
	end := start.Add(2 * time.Second)
	pkt.Set(packet.KeyFirst, first)
	pkt.Set(packet.KeyEnd, end)
	pkt.Set("resp", map[string]any{
		"model": "m1",
		"usage": map[string]any{
			"prompt_tokens":             10.0,
			"completion_tokens":         20.0,
			"total_tokens":              30.0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 5.0},
			"prompt_tokens_details":     map[string]any{"cached_tokens": 4.0},
		},
	})
	pkt.SetPhase(packet.PhaseResp)
	// 模拟 source=openai（dispatcher 回投后设置）
	pkt.Set("source", "openai")

	out := s.Handle(pkt)
	if out == nil {
		t.Error("recorder should return the packet")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	recs, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	// tags
	if r.Tags["provider"] != "openai" || r.Tags["model"] != "m1" || r.Tags["stream"] != "true" || r.Tags["status"] != "ok" {
		t.Errorf("system tags wrong: %v", r.Tags)
	}
	// 用户 metadata 平铺
	if r.Tags["meta_team"] != "core" || r.Tags["meta_n"] != "3" {
		t.Errorf("metadata flatten wrong: %v", r.Tags)
	}
	if _, ok := r.Tags["meta_nested"]; ok {
		t.Error("nested metadata should be ignored")
	}
	// fields
	if r.Fields["prompt_tokens"] != 10.0 || r.Fields["completion_tokens"] != 20.0 ||
		r.Fields["total_tokens"] != 30.0 || r.Fields["reasoning_tokens"] != 5.0 || r.Fields["cached_tokens"] != 4.0 {
		t.Errorf("token fields wrong: %v", r.Fields)
	}
	// cost = 10/1e6*1 + 20/1e6*2 = 0.00005
	if r.Fields["cost"] != 0.00005 {
		t.Errorf("cost wrong: %v", r.Fields["cost"])
	}
	if r.Fields["latency_ms"].(float64) < 0 {
		t.Error("latency wrong")
	}
	if got := r.Fields["latency_ms"].(float64); got < 1900 || got > 2100 {
		t.Errorf("legacy latency should remain full request latency: %v", got)
	}
	if got := r.Fields["ttft_ms"].(float64); got != 250 {
		t.Errorf("ttft wrong: %v", got)
	}
	if got := r.Fields["generation_ms"].(float64); got != 1750 {
		t.Errorf("generation time wrong: %v", got)
	}
	if r.Fields["ttft_samples"] != 1.0 || r.Fields["generation_samples"] != 1.0 {
		t.Errorf("timing samples wrong: %v", r.Fields)
	}
}

func TestSpendRecorderResponsesUsage(t *testing.T) {
	back := mem.NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	recorder := NewSpendRecorder(ts, nil)
	pkt := packet.NewReq(map[string]any{"model": "gpt-5.6-luna"})
	pkt.Set("source", "chatgpt_codex")
	pkt.Set("resp", map[string]any{
		"model": "gpt-5.6-luna",
		"usage": map[string]any{
			"input_tokens":          100.0,
			"output_tokens":         25.0,
			"total_tokens":          125.0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 10.0},
			"input_tokens_details":  map[string]any{"cached_tokens": 80.0},
		},
	})
	pkt.SetPhase(packet.PhaseResp)
	recorder.Handle(pkt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	recs, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	fields := recs[0].Fields
	if fields["prompt_tokens"] != 100.0 || fields["completion_tokens"] != 25.0 || fields["total_tokens"] != 125.0 || fields["reasoning_tokens"] != 10.0 || fields["cached_tokens"] != 80.0 {
		t.Errorf("Responses usage fields wrong: %v", fields)
	}
}

func TestScalarString(t *testing.T) {
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{"abc", "abc", true},
		{3.0, "3", true},
		{3.5, "3.5", true},
		{true, "true", true},
		{map[string]any{"a": 1}, "", false},
		{[]any{1}, "", false},
	}
	for _, c := range cases {
		got, ok := scalarString(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("scalarString(%v) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSpendRecorderProviderCacheHitField(t *testing.T) {
	back := mem.NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	recorder := NewSpendRecorder(ts, nil)
	pkt := packet.NewReq(map[string]any{"model": "m1"})
	pkt.Set("source", "openai")
	pkt.Set("resp", map[string]any{
		"model": "m1",
		"usage": map[string]any{
			"prompt_tokens":           20.0,
			"completion_tokens":       5.0,
			"total_tokens":            25.0,
			"prompt_cache_hit_tokens": 7.0,
		},
	})
	pkt.SetPhase(packet.PhaseResp)
	recorder.Handle(pkt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Fields["cached_tokens"] != 7.0 {
		t.Fatalf("provider cache hit was not normalized: %+v", records)
	}
}

// approxEqual 金额比较：浮点加法结合律会让数学上相等的表达式差最后几个 bit，
// 用固定容差比较（1e-12 USD，远小于任何真实计费精度）。
func approxEqual(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-12
}

func TestPricingSplitsCachedInput(t *testing.T) {
	price := Pricing{InputPerMtok: 2.5, OutputPerMtok: 10, CachedInputPerMtok: 1.25}
	// 1000 prompt 里 400 命中缓存：600*2.5 + 400*1.25 + 100*10 (单位 1e-6 USD)
	want := 600.0/1e6*2.5 + 400.0/1e6*1.25 + 100.0/1e6*10
	if got := price.Cost(1000, 400, 100); !approxEqual(got, want) {
		t.Fatalf("cached split wrong: got %v want %v", got, want)
	}
	// 全价对照：不拆缓存会多收 400*1.25/1e6
	if got := price.Cost(1000, 0, 100); !approxEqual(got, 1000.0/1e6*2.5+100.0/1e6*10) {
		t.Fatalf("no-cache cost wrong: got %v", got)
	}

	// 未配置缓存价 → 回退普通输入价（保守，不因漏配而低估成本）
	noCachePrice := Pricing{InputPerMtok: 2.5, OutputPerMtok: 10}
	full := 1000.0/1e6*2.5 + 100.0/1e6*10
	if got := noCachePrice.Cost(1000, 400, 100); !approxEqual(got, full) {
		t.Fatalf("unset cached price must fall back to input price: got %v want %v", got, full)
	}

	// 上游 usage 不一致：cached > prompt 时夹到 prompt，成本不能变负
	if got, want := price.Cost(100, 500, 0), price.Cost(100, 100, 0); !approxEqual(got, want) {
		t.Fatalf("cached must clamp to prompt: got %v want %v", got, want)
	}

	// 预留取上界：缓存价高于普通输入价时不能低估
	premium := Pricing{InputPerMtok: 1, OutputPerMtok: 2, CachedInputPerMtok: 9}
	if got, wantBound := premium.CostUpperBound(1000, 0), 1000.0/1e6*9; !approxEqual(got, wantBound) {
		t.Fatalf("upper bound must use the larger input unit: got %v want %v", got, wantBound)
	}
}

// TestSpendRecorderCostUsesCachedPrice 守住"录制的 cost 字段真的按缓存价算了"。
// 这是最容易静默回归的地方：公式写对了但 spend 里仍用旧写法。
func TestSpendRecorderCostUsesCachedPrice(t *testing.T) {
	back := mem.NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	recorder := NewSpendRecorder(ts, map[string]Pricing{
		"m1": {InputPerMtok: 2.5, OutputPerMtok: 10, CachedInputPerMtok: 1.25},
	})
	pkt := packet.NewReq(map[string]any{"model": "m1"})
	pkt.Set("source", "openai")
	pkt.Set("resp", map[string]any{
		"model": "m1",
		"usage": map[string]any{
			"prompt_tokens":     1000.0,
			"completion_tokens": 100.0,
			"total_tokens":      1100.0,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 400.0,
			},
		},
	})
	pkt.SetPhase(packet.PhaseResp)
	recorder.Handle(pkt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	want := 600.0/1e6*2.5 + 400.0/1e6*1.25 + 100.0/1e6*10
	got, _ := records[0].Fields["cost"].(float64)
	if !approxEqual(got, want) {
		t.Fatalf("recorded cost must use the cached input price: got %v want %v (cached field %v)",
			got, want, records[0].Fields["cached_tokens"])
	}
	if records[0].Fields["cached_tokens"] != 400.0 {
		t.Fatalf("cached_tokens not recorded: %+v", records[0].Fields)
	}
}

// TestPricingCachedInputFree 显式"缓存免费"开关：命中部分成本为 0。
// 这个开关存在的意义就是不让 0 兼任"未配置"和"免费"两种口径。
func TestPricingCachedInputFree(t *testing.T) {
	price := Pricing{InputPerMtok: 2.5, OutputPerMtok: 10, CachedInputFree: true}
	// 1000 prompt 中 400 免费：只剩 600 按输入价 + 输出
	want := 600.0/1e6*2.5 + 100.0/1e6*10
	if got := price.Cost(1000, 400, 100); !approxEqual(got, want) {
		t.Fatalf("free cache reads must cost nothing: got %v want %v", got, want)
	}
	// 全部命中 → 只有输出成本
	if got := price.Cost(1000, 1000, 100); !approxEqual(got, 100.0/1e6*10) {
		t.Fatalf("all-cached call must only cost output: got %v", got)
	}
	// 免费时预留上界仍是普通输入价（0 不该把上界拉低成 0）
	if got := price.CostUpperBound(1000, 0); !approxEqual(got, 1000.0/1e6*2.5) {
		t.Fatalf("upper bound must stay at the input price when cache is free: got %v", got)
	}
	// 免费开关优先于缓存价（互斥配置在 config.Build 已拦截，这里只固定优先级语义）
	both := Pricing{InputPerMtok: 2.5, OutputPerMtok: 10, CachedInputPerMtok: 1.25, CachedInputFree: true}
	if got := both.CachedInputPrice(); got != 0 {
		t.Fatalf("free switch must win over a numeric price: got %v", got)
	}
}
