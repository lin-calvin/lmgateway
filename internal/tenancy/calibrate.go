package tenancy

import (
	"context"
	"log"
	"time"

	"lmgateway/internal/store"
)

// Calibrate 从 spend 记录重建当日/当月成本计数器。
//
// 为什么需要它:配额计数器是进程内的,重启会归零。若不校准,重启即可绕过
// 日/月预算限制。启动时调用一次,之后由上层定期调用(默认 5 分钟)。
//
// 依赖 spend 记录里带 tenant/project/key 标签(由 handler.SpendRecorder 写入)。
// 缺少标签时本函数安静地什么都不做,不会误判。
func Calibrate(ctx context.Context, ts store.TSStore, limiter *Limiter) error {
	if ts == nil || limiter == nil {
		return nil
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	records, err := ts.Query(ctx, store.TSQuery{Stream: "spend", From: monthStart, To: now})
	if err != nil {
		return err
	}

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	type totals struct{ daily, monthly float64 }
	byScope := map[string]*totals{}

	for _, rec := range records {
		cost, ok := numberField(rec.Fields["cost"])
		if !ok || cost == 0 {
			continue
		}
		inDay := !rec.Ts.Before(dayStart)
		// 一次记录的三个层级都要累计(单个请求同时计入 key/project/tenant)
		for _, tag := range []string{"key", "project", "tenant"} {
			value := rec.Tags[tag]
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
		log.Printf("[tenancy] quota counters calibrated from %d spend records (%d scopes)", len(records), len(byScope))
	}
	return nil
}

func numberField(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
