package store

import (
	"sort"
	"strconv"
	"time"
)

// AggregateRecords 在内存中对记录做聚合（时间桶 + group by tag + 求和）。
// 后端只需按 stream/时间/where 取回记录，聚合逻辑复用这份纯函数。
func AggregateRecords(records []*TSRecord, q TSAgg) []TSAggRow {
	rows := map[string]*TSAggRow{}
	for _, r := range records {
		if !matchAgg(r, q) {
			continue
		}
		key := aggKey(r, q)
		row, ok := rows[key]
		if !ok {
			row = &TSAggRow{
				Tags:   aggTags(r, q.GroupBy),
				Bucket: bucketOf(r.Ts, q.Bucket),
				Sums:   map[string]float64{},
			}
			rows[key] = row
		}
		row.Count++
		for _, f := range q.Sum {
			row.Sums[f] += fieldFloat(r, f)
		}
	}
	var out []TSAggRow
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket.Before(out[j].Bucket) })
	return out
}

// AggMatch 判断记录是否命中聚合过滤（stream/时间/where tag）
func AggMatch(r *TSRecord, q TSAgg) bool {
	return matchAgg(r, q)
}

func matchAgg(r *TSRecord, q TSAgg) bool {
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if !q.From.IsZero() && r.Ts.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && r.Ts.After(q.To) {
		return false
	}
	for k, v := range q.Where {
		if r.Tags[k] != v {
			return false
		}
	}
	return true
}

func aggKey(r *TSRecord, q TSAgg) string {
	k := bucketOf(r.Ts, q.Bucket).String()
	for _, g := range q.GroupBy {
		k += "|" + g + "=" + r.Tags[g]
	}
	return k
}

func aggTags(r *TSRecord, group []string) map[string]string {
	if len(group) == 0 {
		return nil
	}
	t := map[string]string{}
	for _, g := range group {
		t[g] = r.Tags[g]
	}
	return t
}

func bucketOf(t time.Time, d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	return t.Truncate(d)
}

func fieldFloat(r *TSRecord, f string) float64 {
	switch v := r.Fields[f].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0
}
