// Package spendagg 把两个 spend 数据源合并成一个统一视图：
//
//	"spend"        原始流水，只保留最近 raw_retention_days（rollup 会裁剪旧数据）
//	"spend_daily"  rollup 产出的日汇总，覆盖被裁剪掉的历史
//
// 两者按时间**不重叠地**拼接：日汇总覆盖 [.., cutoff)，原始流水覆盖 [cutoff, ..]。
// cutoff 取 rollup 的 watermark（它正是"原始流水已被裁剪到哪一刻"的边界），
// 只在拿不到 watermark 时退化为 now - raw_retention_days。
//
// 为什么不用 now - retention：rollup 是周期性跑的，两个 cutoff 之间可能夹着
// 一段"还没被汇总、也还没被删除"的原始流水。用当前时间算分界点会把这段算漏。
package spendagg

import (
	"context"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/store"
)

// StreamRaw / StreamDaily 两个数据流名。
const (
	StreamRaw   = "spend"
	StreamDaily = "spend_daily"
)

// Split 一次查询在 raw / daily 上的不重叠区间。
type Split struct {
	From, To time.Time

	// Cutoff 分界点（左闭右开：daily 到 Cutoff 为止，raw 从 Cutoff 开始）。
	Cutoff time.Time

	RawFrom, RawTo     time.Time
	DailyFrom, DailyTo time.Time
}

// Plan 计算区间。cutoff 传零值表示"没有 rollup 信息"，此时全部走 raw。
func Plan(from, to, cutoff time.Time) Split {
	s := Split{From: from, To: to}
	switch {
	case cutoff.IsZero() || !cutoff.After(from):
		// 没有历史汇总：raw 覆盖整个区间。
		s.RawFrom, s.RawTo = from, to
	case !cutoff.Before(to):
		// 分界点在区间右侧：整段都来自历史汇总。
		s.Cutoff = to
		s.DailyFrom, s.DailyTo = from, to
	default:
		s.Cutoff = cutoff
		s.DailyFrom, s.DailyTo = from, cutoff
		s.RawFrom, s.RawTo = cutoff, to
	}
	return s
}

// Item 合并后的一条记录。raw 记录 Requests=1；daily 行 Requests 取 Fields["requests"]。
type Item struct {
	Ts       time.Time
	Tags     map[string]string
	Fields   map[string]any
	Requests float64
	Daily    bool
}

// UsedRaw / UsedDaily 判断 Split 是否覆盖了某一侧。
func (s Split) UsedRaw() bool   { return s.RawFrom.Before(s.RawTo) }
func (s Split) UsedDaily() bool { return s.DailyFrom.Before(s.DailyTo) }

// Source 返回与 internal/api TokenSummary 一致的来源标记：raw|daily|mixed|empty。
func (s Split) Source() string {
	switch {
	case s.UsedRaw() && s.UsedDaily():
		return "mixed"
	case s.UsedRaw():
		return "raw"
	case s.UsedDaily():
		return "daily"
	default:
		return "empty"
	}
}

// Collect 查询 raw + daily 并合并。rawTags/dailyTags 是下推到存储的等值过滤：
// dailyTags 必须只包含日汇总里确实存在的 tag（否则会把整段历史过滤成空）。
func Collect(ctx context.Context, ts store.TSStore, s Split, rawTags, dailyTags map[string]string) ([]Item, error) {
	var out []Item
	if ts == nil {
		return out, nil
	}
	if s.UsedDaily() {
		recs, err := ts.Query(ctx, store.TSQuery{
			Stream: StreamDaily,
			From:   s.DailyFrom,
			To:     s.DailyTo,
			Tag:    dailyTags,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			requests := Number(r.Fields["requests"])
			if requests <= 0 {
				requests = 1
			}
			out = append(out, Item{Ts: r.Ts, Tags: r.Tags, Fields: r.Fields, Requests: requests, Daily: true})
		}
	}
	if s.UsedRaw() {
		recs, err := ts.Query(ctx, store.TSQuery{
			Stream: StreamRaw,
			From:   s.RawFrom,
			To:     s.RawTo,
			Tag:    rawTags,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			out = append(out, Item{Ts: r.Ts, Tags: r.Tags, Fields: r.Fields, Requests: 1})
		}
	}
	return out, nil
}

// EffectiveCutoff 选出用于 Plan 的分界点。
//
// watermark 来自 rollup，语义是"原始流水已被裁剪到这一刻"。它是唯一可靠的
// 分界依据：拿不到时就当作"从未汇总过"，整段走 raw——因为 rollup 只有成功跑完
// 才会裁剪数据，没有 watermark 就意味着 raw 仍然完整（用 now-retention 反而会
// 把还没被汇总、也还在库里的历史当成已被裁剪而漏算）。
func EffectiveCutoff(watermark time.Time) time.Time {
	return watermark
}

// Number 把字段值统一转成 float64（兼容 JSON 反序列化后的各种数字类型与字符串）。
func Number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f
		}
	}
	return 0
}

// HasDim 判断 tag 维度是否可用。
func HasDim(dims []string, dim string) bool {
	for _, d := range dims {
		if d == dim {
			return true
		}
	}
	return false
}
