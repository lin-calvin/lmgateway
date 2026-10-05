package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/spendagg"
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

func (a *api) handleToken(w http.ResponseWriter, r *http.Request) {
	sp := a.deps.Manager.Runtime().Config.Spend
	defaults := config.DefaultSpendCfg()
	if sp.RawRetentionDays <= 0 {
		sp.RawRetentionDays = defaults.RawRetentionDays
	}
	if sp.Timezone == "" {
		sp.Timezone = defaults.Timezone
	}
	loc, err := time.LoadLocation(sp.Timezone)
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
	case "7d", "30d":
		days := 7
		if label == "30d" {
			days = 30
		}
		return now.AddDate(0, 0, -days).UTC(), nowUTC, nil
	default:
		date, err := time.ParseInLocation("2006-01-02", label, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported time %q (use today, yesterday, 7d, 30d, or YYYY-MM-DD)", label)
		}
		return dayStart(date).UTC(), dayStart(date).AddDate(0, 0, 1).UTC(), nil
	}
}

func (a *api) queryTokenSummary(r *http.Request, label string, sp config.SpendCfg, from, to time.Time) (TokenSummary, error) {
	// 分界点取 rollup watermark：它精确表示"原始流水已被裁剪到哪一刻"，
	// 比 now-raw_retention 更准（rollup 是周期性跑的，两者之间会夹着一段数据）。
	var wm time.Time
	if a.deps.Rollup != nil {
		wm, _ = a.deps.Rollup.Watermark(r.Context())
	}
	split := spendagg.Plan(from, to, spendagg.EffectiveCutoff(wm))
	result := TokenSummary{Time: label}
	usedRaw, usedDaily := false, false

	if split.UsedRaw() {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: spendagg.StreamRaw,
			From:   split.RawFrom,
			To:     split.RawTo,
		})
		if err != nil {
			return result, err
		}
		addRawRecords(&result, recs)
		usedRaw = len(recs) > 0
	}

	if split.UsedDaily() {
		recs, err := a.deps.TS.Query(r.Context(), store.TSQuery{
			Stream: spendagg.StreamDaily,
			From:   split.DailyFrom,
			To:     split.DailyTo,
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
