package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/keytag"
	"lmgateway/internal/store"
)

// TokenSummary 是面向业务的 token 用量汇总；不计算 cache 命中率，只返回命中 token 数。
type TokenSummary struct {
	Time     string         `json:"time"`
	Timezone string         `json:"timezone"`
	From     time.Time      `json:"from"`
	To       time.Time      `json:"to"`
	Source   string         `json:"source"` // raw | daily | mixed | empty
	Requests int64          `json:"requests"`
	Tokens   TokenCounts    `json:"tokens"`
	Cost     float64        `json:"cost"`
	Latency  LatencySummary `json:"latency_ms"`
	Timing   TimingSummary  `json:"timing_ms"`

	ttftSamples       float64
	generationSamples float64
	generationTokens  float64
}

type TokenCounts struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type LatencySummary struct {
	Sum float64 `json:"sum"`
	Avg float64 `json:"avg"`
}

type TimingSummary struct {
	TTFT                      LatencySummary `json:"ttft_ms"`
	Generation                LatencySummary `json:"generation_ms"`
	GenerationTokensPerSecond float64        `json:"generation_tokens_per_second"`
}

func (a *api) spendSettings() (config.SpendCfg, *time.Location, error) {
	sp := a.deps.Manager.Runtime().Config.Spend
	defaults := config.DefaultSpendCfg()
	if sp.RawRetentionDays <= 0 {
		sp.RawRetentionDays = defaults.RawRetentionDays
	}
	if sp.Timezone == "" {
		sp.Timezone = defaults.Timezone
	}
	loc, err := time.LoadLocation(sp.Timezone)
	return sp, loc, err
}

func (a *api) handleToken(w http.ResponseWriter, r *http.Request) {
	sp, loc, err := a.spendSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "invalid spend timezone: "+err.Error())
		return
	}

	label := r.URL.Query().Get("time")
	if label == "" {
		label = "today"
	}
	from, to, err := tokenWindow(label, time.Now().UTC(), loc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := a.queryTokenSummary(r, label, sp, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	result.Timezone = sp.Timezone
	result.From = from
	result.To = to
	writeJSON(w, http.StatusOK, result)
}

// TagSummaryRow is one client-declared key_tag bucket.
type TagSummaryRow struct {
	Tag      string      `json:"tag"`
	Requests int64       `json:"requests"`
	Tokens   TokenCounts `json:"tokens"`
	Cost     float64     `json:"cost"`
}

// TagSummary is the by-tag breakdown over the same window as /api/spend/token.
type TagSummary struct {
	Time string          `json:"time"`
	From time.Time       `json:"from"`
	To   time.Time       `json:"to"`
	Rows []TagSummaryRow `json:"rows"`
}

// SeriesPoint is one time bucket of the spend trend.
type SeriesPoint struct {
	T        time.Time   `json:"t"`
	Requests int64       `json:"requests"`
	Tokens   TokenCounts `json:"tokens"`
	Cost     float64     `json:"cost"`
}

// Series is the token/cost/request trend over a spend window.
type Series struct {
	Time   string        `json:"time"`
	From   time.Time     `json:"from"`
	To     time.Time     `json:"to"`
	Bucket string        `json:"bucket"` // 1h | 24h
	Points []SeriesPoint `json:"points"`
}

func (a *api) handleTagSummary(w http.ResponseWriter, r *http.Request) {
	sp, loc, err := a.spendSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "invalid spend timezone: "+err.Error())
		return
	}
	label := r.URL.Query().Get("time")
	if label == "" {
		label = "today"
	}
	from, to, err := tokenWindow(label, time.Now().UTC(), loc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := a.queryTagSummary(r, sp, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, TagSummary{Time: label, From: from, To: to, Rows: rows})
}

// queryTagSummary merges raw spend with rolled-up spend_daily, grouped by
// key_tag. Records without a tag are reported as "(untagged)".
func (a *api) queryTagSummary(r *http.Request, sp config.SpendCfg, from, to time.Time) ([]TagSummaryRow, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(sp.RawRetentionDays) * 24 * time.Hour)
	groups := map[string]*TokenSummary{}
	group := func(tag string) *TokenSummary {
		if tag == "" {
			tag = "(untagged)"
		}
		g := groups[tag]
		if g == nil {
			g = &TokenSummary{}
			groups[tag] = g
		}
		return g
	}

	if rawFrom := maxTime(from, cutoff); rawFrom.Before(to) {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: "spend",
			From:   rawFrom,
			To:     minTime(to, now),
		})
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			addRawRecords(group(rec.Tags[keytag.TagField]), []*store.TSRecord{rec})
		}
	}

	if dailyTo := minTime(to, cutoff); from.Before(dailyTo) {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: "spend_daily",
			From:   from,
			To:     dailyTo,
		})
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			addDailyRecords(group(rec.Tags[keytag.TagField]), []*store.TSRecord{rec})
		}
	}

	rows := make([]TagSummaryRow, 0, len(groups))
	for tag, g := range groups {
		rows = append(rows, TagSummaryRow{Tag: tag, Requests: g.Requests, Tokens: g.Tokens, Cost: g.Cost})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Tokens.TotalTokens != rows[j].Tokens.TotalTokens {
			return rows[i].Tokens.TotalTokens > rows[j].Tokens.TotalTokens
		}
		if rows[i].Cost != rows[j].Cost {
			return rows[i].Cost > rows[j].Cost
		}
		return rows[i].Tag < rows[j].Tag
	})
	return rows, nil
}

// handleSpendSeries returns a complete (gap-filled) trend for the Spend page:
// hourly buckets for a single day, daily buckets for multi-day windows.
func (a *api) handleSpendSeries(w http.ResponseWriter, r *http.Request) {
	sp, loc, err := a.spendSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "invalid spend timezone: "+err.Error())
		return
	}
	label := r.URL.Query().Get("time")
	if label == "" {
		label = "today"
	}
	from, to, err := tokenWindow(label, time.Now().UTC(), loc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	bucket, bucketLabel := seriesBucket(label)
	points, err := a.querySpendSeries(r, sp, from, to, bucket)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, Series{
		Time:   label,
		From:   from,
		To:     to,
		Bucket: bucketLabel,
		Points: points,
	})
}

func seriesBucket(label string) (time.Duration, string) {
	switch label {
	case "today", "yesterday":
		return time.Hour, "1h"
	case "7d", "30d", "90d", "180d", "1y", "365d":
		return 24 * time.Hour, "24h"
	default:
		// A specific YYYY-MM-DD is a single day.
		return time.Hour, "1h"
	}
}

func (a *api) querySpendSeries(r *http.Request, sp config.SpendCfg, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(sp.RawRetentionDays) * 24 * time.Hour)

	type acc struct {
		requests int64
		tokens   int64
		cost     float64
	}
	byBucket := map[time.Time]*acc{}
	add := func(bucket time.Time, requests, tokens int64, cost float64) {
		v := byBucket[bucket]
		if v == nil {
			v = &acc{}
			byBucket[bucket] = v
		}
		v.requests += requests
		v.tokens += tokens
		v.cost += cost
	}

	sums := []string{"total_tokens", "cost"}
	if rawFrom := maxTime(from, cutoff); rawFrom.Before(to) {
		rows, err := a.deps.TS.Aggregate(r.Context(), store.TSAgg{
			Stream: "spend",
			From:   rawFrom,
			To:     minTime(to, now),
			Bucket: bucket,
			Sum:    sums,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			add(row.Bucket.UTC(), row.Count, int64(row.Sums["total_tokens"]), row.Sums["cost"])
		}
	}

	if dailyTo := minTime(to, cutoff); from.Before(dailyTo) {
		rows, err := a.deps.TS.Aggregate(r.Context(), store.TSAgg{
			Stream: "spend_daily",
			From:   from,
			To:     dailyTo,
			Bucket: 24 * time.Hour,
			Sum:    []string{"total_tokens", "cost", "requests"},
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			requests := int64(row.Sums["requests"])
			if requests == 0 {
				requests = row.Count
			}
			add(row.Bucket.UTC(), requests, int64(row.Sums["total_tokens"]), row.Sums["cost"])
		}
	}

	points := []SeriesPoint{}
	if from.IsZero() || !from.Before(to) {
		return points, nil
	}
	for t := from.UTC().Truncate(bucket); t.Before(to.UTC()); t = t.Add(bucket) {
		point := SeriesPoint{T: t}
		if v := byBucket[t]; v != nil {
			point.Requests = v.requests
			point.Tokens.TotalTokens = v.tokens
			point.Cost = v.cost
		}
		points = append(points, point)
	}
	return points, nil
}

func tokenWindow(label string, nowUTC time.Time, loc *time.Location) (time.Time, time.Time, error) {
	now := nowUTC.In(loc)
	dayStart := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}

	switch label {
	case "today":
		return dayStart(now).UTC(), nowUTC, nil
	case "yesterday":
		start := dayStart(now).AddDate(0, 0, -1)
		return start.UTC(), dayStart(now).UTC(), nil
	case "7d", "30d", "90d", "180d", "1y", "365d":
		days := map[string]int{"7d": 7, "30d": 30, "90d": 90, "180d": 180, "1y": 365, "365d": 365}[label]
		return now.AddDate(0, 0, -days).UTC(), nowUTC, nil
	default:
		date, err := time.ParseInLocation("2006-01-02", label, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported time %q (use today, yesterday, 7d, 30d, 90d, 180d, 1y, or YYYY-MM-DD)", label)
		}
		return dayStart(date).UTC(), dayStart(date).AddDate(0, 0, 1).UTC(), nil
	}
}

func (a *api) queryTokenSummary(r *http.Request, label string, sp config.SpendCfg, from, to time.Time) (TokenSummary, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(sp.RawRetentionDays) * 24 * time.Hour)
	result := TokenSummary{Time: label}
	usedRaw, usedDaily := false, false

	if rawFrom := maxTime(from, cutoff); rawFrom.Before(to) {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: "spend",
			From:   rawFrom,
			To:     minTime(to, now),
		})
		if err != nil {
			return result, err
		}
		addRawRecords(&result, recs)
		usedRaw = len(recs) > 0
	}

	if dailyTo := minTime(to, cutoff); from.Before(dailyTo) {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: "spend_daily",
			From:   from,
			To:     dailyTo,
		})
		if err != nil {
			return result, err
		}
		addDailyRecords(&result, recs)
		usedDaily = len(recs) > 0
	}

	switch {
	case usedRaw && usedDaily:
		result.Source = "mixed"
	case usedRaw:
		result.Source = "raw"
	case usedDaily:
		result.Source = "daily"
	default:
		result.Source = "empty"
	}
	if result.Requests > 0 {
		result.Latency.Avg = result.Latency.Sum / float64(result.Requests)
	}
	if result.ttftSamples > 0 {
		result.Timing.TTFT.Avg = result.Timing.TTFT.Sum / result.ttftSamples
	}
	if result.generationSamples > 0 {
		result.Timing.Generation.Avg = result.Timing.Generation.Sum / result.generationSamples
		if result.Timing.Generation.Sum > 0 {
			result.Timing.GenerationTokensPerSecond = result.generationTokens / (result.Timing.Generation.Sum / 1000)
		}
	}
	return result, nil
}

func addRawRecords(out *TokenSummary, records []*store.TSRecord) {
	for _, r := range records {
		out.Requests++
		addFields(out, r.Fields, false)
	}
}

func addDailyRecords(out *TokenSummary, records []*store.TSRecord) {
	for _, r := range records {
		requests := number(r.Fields["requests"])
		if requests <= 0 {
			requests = 1
		}
		out.Requests += int64(requests)
		addFields(out, r.Fields, true)
	}
}

func addFields(out *TokenSummary, fields map[string]any, _ bool) {
	out.Tokens.PromptTokens += int64(number(fields["prompt_tokens"]))
	out.Tokens.CompletionTokens += int64(number(fields["completion_tokens"]))
	out.Tokens.ReasoningTokens += int64(number(fields["reasoning_tokens"]))
	out.Tokens.CachedTokens += int64(number(fields["cached_tokens"]))
	out.Tokens.TotalTokens += int64(number(fields["total_tokens"]))
	out.Cost += number(fields["cost"])
	out.Latency.Sum += number(fields["latency_ms"])
	out.Timing.TTFT.Sum += number(fields["ttft_ms"])
	out.Timing.Generation.Sum += number(fields["generation_ms"])
	out.ttftSamples += number(fields["ttft_samples"])
	out.generationSamples += number(fields["generation_samples"])
	out.generationTokens += number(fields["generation_tokens"])
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	default:
		return 0
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
