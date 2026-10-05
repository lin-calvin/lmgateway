package tenancy

import (
	"context"
	"log"
	"time"

	"lmgateway/internal/spendagg"
	"lmgateway/internal/store"
)

// Calibrate 从 spend 记录重建当日/当月成本计数器。
//
// 为什么需要它:配额计数器是进程内的,重启会归零。若不校准,重启即可绕过
// 日/月预算限制。启动时调用一次,之后由上层定期调用(默认 5 分钟)。
//
// 数据来源是 raw spend + spend_daily 的合并:raw 超过 raw_retention_days 就被
// rollup 裁剪,只看 raw 会让"月中重启"把当月已花成本算漏(等于凭空恢复预算)。
// watermark 是 rollup 的裁剪边界,由上层注入;为 nil 时退化为只看 raw。
//
// 依赖 spend 记录里带 tenant/project/key 标签(由 handler.SpendRecorder 写入,
// 且已列入 rollup 强制维度,历史汇总同样带这些标签)。
func Calibrate(ctx context.Context, ts store.TSStore, limiter *Limiter, watermark func(context.Context) (time.Time, error)) error {
	if ts == nil || limiter == nil {
		return nil
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var wm time.Time
	if watermark != nil {
		if value, err := watermark(ctx); err == nil {
			wm = value
		}
	}
	split := spendagg.Plan(monthStart, now, spendagg.EffectiveCutoff(wm))
	items, err := spendagg.Collect(ctx, ts, split, nil, nil)
	if err != nil {
		return err
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	type totals struct{ daily, monthly float64 }
	byScope := map[string]*totals{}

	for _, item := range items {
		cost := spendagg.Number(item.Fields["cost"])
		if cost == 0 {
			continue
		}
		inDay := !item.Ts.Before(dayStart)
		// 一次记录的三个层级都要累计(单个请求同时计入 key/project/tenant)
		for _, tag := range []string{"key", "project", "tenant"} {
			value := item.Tags[tag]
			if value == "" {
				continue
			}
			scope := tag + ":" + value
			total, ok := byScope[scope]
			if !ok {
				total = &totals{}
				byScope[scope] = total
			}
			total.monthly += cost
			if inDay {
				total.daily += cost
			}
		}
	}

	for scope, total := range byScope {
		limiter.Load(scope, total.daily, total.monthly, 0)
	}
	if len(byScope) > 0 {
		// 没有 rollup watermark 时 cutoff 是零值：说清楚"全量 raw"比打印 0001-01-01 有用。
		cutoff := "raw-only (no rollup watermark)"
		if !split.Cutoff.IsZero() {
			cutoff = split.Cutoff.Format(time.RFC3339)
		}
		log.Printf("[tenancy] quota counters calibrated from %d spend rows (%d scopes, cutoff=%s)",
			len(items), len(byScope), cutoff)
	}
	return nil
}
