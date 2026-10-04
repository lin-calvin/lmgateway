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
