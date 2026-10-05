package spendagg

import (
	"context"
	"testing"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestPlanSplitsRawAndDailyWithoutOverlap(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	s := Plan(from, to, EffectiveCutoff(cutoff))
	if s.Source() != "mixed" {
		t.Fatalf("source = %q, want mixed", s.Source())
	}
	// 关键不变量：两侧首尾相接但不重叠。
	if !s.DailyTo.Equal(cutoff) {
		t.Fatalf("dailyTo = %s, want %s", s.DailyTo, cutoff)
	}
	if !s.RawFrom.Equal(cutoff) {
		t.Fatalf("rawFrom = %s, want %s", s.RawFrom, cutoff)
	}
	if s.DailyTo.After(s.RawFrom) {
		t.Fatalf("windows overlap: daily ends %s, raw starts %s", s.DailyTo, s.RawFrom)
	}

	// 没有 watermark（rollup 从未成功跑过）→ 全部走 raw，不能漏掉仍在库里的历史。
	all := Plan(from, to, EffectiveCutoff(time.Time{}))
	if all.Source() != "raw" || !all.RawFrom.Equal(from) || !all.RawTo.Equal(to) {
		t.Fatalf("no watermark must read all raw, got %+v (source=%s)", all, all.Source())
	}

	// 分界点在区间右侧 → 整段来自历史汇总。
	old := Plan(from, to, cutoff.AddDate(0, 1, 0))
	if old.Source() != "daily" {
		t.Fatalf("cutoff beyond range should be daily-only, got %s", old.Source())
	}

	// 分界点在区间左侧 → 整段来自 raw。
	recent := Plan(from, to, from.Add(-time.Hour))
	if recent.Source() != "raw" {
		t.Fatalf("cutoff before range should be raw-only, got %s", recent.Source())
	}
}

func TestCollectMergesRawAndDaily(t *testing.T) {
	backend := mem.NewTSBackend()
	ts := store.NewTS(backend, store.BufferedOpts{})
	ctx := context.Background()
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	// 历史：已汇总，一条日行代表 4 个请求。
	if err := ts.Append(ctx, &store.TSRecord{
		Ts: cutoff.Add(-48 * time.Hour), Stream: store.StreamSpendDaily,
		Tags:   map[string]string{"day": "2026-10-03", "tenant": "acme", "provider": "openai"},
		Fields: map[string]any{"requests": float64(4), "total_tokens": float64(400), "cost": 1.5},
	}); err != nil {
		t.Fatal(err)
	}
	// 近期：原始流水两条。
	for i := 0; i < 2; i++ {
		if err := ts.Append(ctx, &store.TSRecord{
			Ts: cutoff.Add(time.Duration(i) * time.Hour), Stream: store.StreamSpend,
			Tags:   map[string]string{"tenant": "acme", "provider": "openai"},
			Fields: map[string]any{"total_tokens": float64(10), "cost": 0.25},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	split := Plan(from, to, cutoff)
	items, err := Collect(ctx, ts, split, map[string]string{"tenant": "acme"}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var requests, tokens, cost float64
	for _, item := range items {
		requests += item.Requests
		tokens += Number(item.Fields["total_tokens"])
		cost += Number(item.Fields["cost"])
	}
	if requests != 6 {
		t.Fatalf("requests = %v, want 6 (4 daily + 2 raw)", requests)
	}
	if tokens != 420 {
		t.Fatalf("tokens = %v, want 420", tokens)
	}
	if cost != 2.0 {
		t.Fatalf("cost = %v, want 2.0", cost)
	}
}

func TestNumberHandlesJSONShapes(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5}, {int(2), 2}, {int64(3), 3}, {"4.5", 4.5}, {"nope", 0}, {nil, 0},
	}
	for _, c := range cases {
		if got := Number(c.in); got != c.want {
			t.Fatalf("Number(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
