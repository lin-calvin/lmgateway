// Package rollup 实现 spend 热/冷分层：把过期 raw 按 (day × rollup_dimensions) 聚合成日汇总，
// 删除已处理的 raw（有界表），watermark 保证不重复处理。
//
//	recent(≤ raw_retention) → 原始 spend 记录，任意口径可查
//	历史                 → spend_daily 日汇总，仅日桶 + rollup 维度
package rollup

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
)

// watermarkKey 记录上次已聚合到的 cutoff
const watermarkKey = "setting/rollup_watermark"

type Job struct {
	ts store.TSStore
	js store.JSONStore
}

func New(ts store.TSStore, js store.JSONStore) *Job { return &Job{ts: ts, js: js} }

// Start 启动：先跑一次，再按 interval 周期跑
func (j *Job) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		_ = j.Run(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = j.Run(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Run 一次聚合：把 [watermark, cutoff) 的 raw 汇总进 spend_daily，删旧 raw，推进 watermark。
// 崩溃一致性（简化）：append→flush→删 raw→写 watermark；极端时序下可能重复 append，
// 但因每个 (day×dims) 的多个部分行会被聚合求和，重复只会略微高估，不会漏记。
func (j *Job) Run(ctx context.Context) error {
	sp := j.settings(ctx)
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(sp.RawRetentionDays) * 24 * time.Hour)

	wm, err := j.watermark(ctx)
	if err != nil {
		return err
	}
	if !wm.IsZero() && !cutoff.After(wm) {
		return nil // 未到下一个窗口
	}

	recs, err := j.ts.Query(ctx, store.TSQuery{Stream: "spend", From: wm, To: cutoff})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		// 没有可聚合数据，也要推进 watermark，避免每次空跑
		if _, err := j.js.Put(ctx, watermarkKey, cutoff); err != nil {
			return err
		}
		return nil
	}

	groups := aggregate(recs, sp.RollupDimensions)
	if err := j.appendDaily(ctx, groups); err != nil {
		return err
	}
	if err := j.ts.Flush(ctx); err != nil {
		return err
	}
	if _, err := j.ts.DeleteByQuery(ctx, "spend", cutoff); err != nil {
		return err
	}
	if _, err := j.js.Put(ctx, watermarkKey, cutoff); err != nil {
		return err
	}

	log.Printf("[rollup] %d raw → %d daily rows, cutoff=%s", len(recs), len(groups), cutoff.Format(time.RFC3339))
	return nil
}

func (j *Job) settings(ctx context.Context) config.SpendCfg {
	sp := config.DefaultSpendCfg()
	if sd, err := j.js.Get(ctx, config.SpendSettingKey); err == nil {
		var cfg config.SpendCfg
		if err := json.Unmarshal(sd.Data, &cfg); err == nil {
			if cfg.RawRetentionDays > 0 {
				sp.RawRetentionDays = cfg.RawRetentionDays
			}
			if len(cfg.RollupDimensions) > 0 {
				sp.RollupDimensions = cfg.RollupDimensions
			}
		}
	}
	return sp
}

func (j *Job) watermark(ctx context.Context) (time.Time, error) {
	w, err := j.js.Get(ctx, watermarkKey)
	if err == store.ErrNotFound {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	if err := json.Unmarshal(w.Data, &t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func (j *Job) appendDaily(ctx context.Context, groups []*dailyAgg) error {
	for _, g := range groups {
		if err := j.ts.Append(ctx, &store.TSRecord{
			Ts:     g.day,
			Stream: "spend_daily",
			Tags:   g.dims,
			Fields: map[string]any{
				"requests":           float64(g.requests),
				"prompt_tokens":      g.prompt,
				"completion_tokens":  g.completion,
				"total_tokens":       g.total,
				"reasoning_tokens":   g.reasoning,
				"cached_tokens":      g.cached,
				"cost":               g.cost,
				"latency_ms":         g.latency,
				"ttft_ms":            g.ttft,
				"ttft_samples":       g.ttftSamples,
				"generation_ms":      g.generation,
				"generation_samples": g.generationSamples,
				"generation_tokens":  g.generationTokens,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

type dailyAgg struct {
	day                                                                time.Time
	dims                                                               map[string]string
	requests                                                           int64
	prompt, completion                                                 float64
	total, reasoning, cached, cost, latency                            float64
	ttft, ttftSamples, generation, generationSamples, generationTokens float64
}

// aggregate 按 (day × dims) 分组汇总 raw 记录
func aggregate(recs []*store.TSRecord, dims []string) []*dailyAgg {
	byKey := map[string]*dailyAgg{}
	var order []string
	for _, r := range recs {
		day := r.Ts.UTC().Truncate(24 * time.Hour)
		key := day.String()
		tags := map[string]string{"day": day.Format("2006-01-02")}
		for _, d := range dims {
			tags[d] = r.Tags[d]
			key += "|" + d + "=" + r.Tags[d]
		}
		g, ok := byKey[key]
		if !ok {
			g = &dailyAgg{day: day, dims: tags}
			byKey[key] = g
			order = append(order, key)
		}
		g.requests++
		g.prompt += f(r, "prompt_tokens")
		g.completion += f(r, "completion_tokens")
		g.total += f(r, "total_tokens")
		g.reasoning += f(r, "reasoning_tokens")
		g.cached += f(r, "cached_tokens")
		g.cost += f(r, "cost")
		g.latency += f(r, "latency_ms")
		g.ttft += f(r, "ttft_ms")
		g.ttftSamples += f(r, "ttft_samples")
		g.generation += f(r, "generation_ms")
		g.generationSamples += f(r, "generation_samples")
		g.generationTokens += f(r, "generation_tokens")
	}
	out := make([]*dailyAgg, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func f(r *store.TSRecord, field string) float64 {
	switch v := r.Fields[field].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if fv, err := strconv.ParseFloat(v, 64); err == nil {
			return fv
		}
	}
	return 0
}
