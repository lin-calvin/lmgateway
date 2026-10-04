package rollup

import (
	"context"
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func seedRaw(t *testing.T, back *mem.TSBackend, recs []*store.TSRecord) {
	t.Helper()
	if err := back.AppendBatch(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, rawRetentionDays int, dims []string) (store.TSStore, store.JSONStore, *mem.TSBackend) {
	t.Helper()
	js := mem.NewJSON()
	ctx := context.Background()
	if _, err := js.Put(ctx, config.SpendSettingKey, config.SpendCfg{
		RawRetentionDays: rawRetentionDays,
		RollupDimensions: dims,
	}); err != nil {
		t.Fatal(err)
	}
	back := mem.NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	t.Cleanup(func() { ts.Close() })
	return ts, js, back
}

func rec(ts time.Time, provider, model, stream string, prompt, completion float64) *store.TSRecord {
	return &store.TSRecord{
		Ts: ts, Stream: "spend",
		Tags: map[string]string{"provider": provider, "model": model, "stream": stream, "status": "ok"},
		Fields: map[string]any{
			"prompt_tokens":      prompt,
			"completion_tokens":  completion,
			"total_tokens":       prompt + completion,
			"reasoning_tokens":   1,
			"cached_tokens":      2,
			"cost":               0.01,
			"latency_ms":         100,
			"ttft_ms":            20,
			"ttft_samples":       1,
			"generation_ms":      80,
			"generation_samples": 1,
			"generation_tokens":  completion,
		},
	}
}

func TestRollupAggregatesAndPrunes(t *testing.T) {
	ts, js, back := setup(t, 7, []string{"provider", "model", "stream"})
	ctx := context.Background()

	now := time.Now().UTC()
	// 过期（> 7 天前）：应被聚合
	old1 := now.AddDate(0, 0, -10)
	old2 := now.AddDate(0, 0, -9)
	// 近期（< 7 天）：应保留在 raw
	recent := now.Add(-time.Hour)

	seedRaw(t, back, []*store.TSRecord{
		rec(old1, "p1", "m1", "false", 10, 5),
		rec(old1, "p1", "m1", "false", 20, 10), // 同 (day,p1,m1,false) 合并
		rec(old1, "p1", "m2", "false", 30, 15),
		rec(old2, "p2", "m1", "true", 40, 20),
		rec(recent, "p1", "m1", "false", 100, 50),
	})

	if err := (&Job{ts: ts, js: js}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 日汇总行
	daily, err := ts.Query(ctx, store.TSQuery{Stream: "spend_daily"})
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 3 { // (day1,p1,m1), (day1,p1,m2), (day2,p2,m1)
		t.Fatalf("expected 3 daily rows, got %d: %+v", len(daily), daily)
	}
	byKey := map[string]*store.TSRecord{}
	for _, d := range daily {
		byKey[d.Tags["model"]+"|"+d.Tags["provider"]+"|"+d.Tags["stream"]] = d
	}
	if r := byKey["m1|p1|false"]; r == nil || r.Fields["requests"] != float64(2) || r.Fields["prompt_tokens"] != float64(30) || r.Fields["completion_tokens"] != float64(15) {
		t.Errorf("m1/p1 daily wrong: %+v", r)
	}
	if r := byKey["m2|p1|false"]; r == nil || r.Fields["prompt_tokens"] != float64(30) {
		t.Errorf("m2/p1 daily wrong: %+v", r)
	}
	if r := byKey["m1|p2|true"]; r == nil || r.Fields["prompt_tokens"] != float64(40) || r.Tags["day"] == "" {
		t.Errorf("m1/p2 daily wrong: %+v", r)
	}

	// raw 只剩近期记录
	raw, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw[0].Fields["prompt_tokens"] != float64(100) {
		t.Errorf("raw should keep only recent record, got %d", len(raw))
	}

	// watermark 已推进
	w, err := js.Get(ctx, watermarkKey)
	if err != nil {
		t.Fatalf("watermark missing: %v", err)
	}
	if len(w.Data) == 0 {
		t.Error("watermark empty")
	}
}

// 第二次 Run 不应重复聚合（watermark 防重）
func TestRollupIdempotent(t *testing.T) {
	ts, js, back := setup(t, 7, []string{"provider", "model", "stream"})
	ctx := context.Background()
	seedRaw(t, back, []*store.TSRecord{
		rec(time.Now().UTC().AddDate(0, 0, -10), "p1", "m1", "false", 10, 5),
	})
	j := &Job{ts: ts, js: js}
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	daily, _ := ts.Query(ctx, store.TSQuery{Stream: "spend_daily"})
	if len(daily) != 1 {
		t.Fatalf("second run should not duplicate, got %d daily rows", len(daily))
	}
	if daily[0].Fields["prompt_tokens"] != float64(10) {
		t.Errorf("duplicated aggregation: %+v", daily[0].Fields)
	}
}

// 配置的 rollup 维度：加 meta_team → 日汇总按 meta_team 分组
func TestRollupConfigurableDimensions(t *testing.T) {
	ts, js, back := setup(t, 7, []string{"provider", "model", "meta_team"})
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -10)
	seedRaw(t, back, []*store.TSRecord{
		rec(old, "p1", "m1", "false", 10, 5),
		rec(old, "p1", "m1", "false", 20, 5),
	})
	// 给两条打上不同 meta_team 标签
	back.Records()[0].Tags["meta_team"] = "core"
	back.Records()[1].Tags["meta_team"] = "infra"

	if err := (&Job{ts: ts, js: js}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	daily, _ := ts.Query(ctx, store.TSQuery{Stream: "spend_daily"})
	if len(daily) != 2 {
		t.Fatalf("expected 2 daily rows grouped by meta_team, got %d: %+v", len(daily), daily)
	}
	teams := map[string]float64{}
	for _, d := range daily {
		teams[d.Tags["meta_team"]] = d.Fields["prompt_tokens"].(float64)
	}
	if teams["core"] != 10 || teams["infra"] != 20 {
		t.Errorf("meta_team grouping wrong: %v", teams)
	}
}
