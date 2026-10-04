package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/store"
)

type Handler struct {
	ts store.TSStore
}

type metricDefinition struct {
	name  string
	help  string
	value func(store.TSAggRow) float64
}

var labelNames = []string{"model", "provider", "status", "stream"}

var metricDefinitions = []metricDefinition{
	{name: "lmgateway_spend_requests_last_5m", help: "Spend requests in the last 5 minutes.", value: func(row store.TSAggRow) float64 { return float64(row.Count) }},
	{name: "lmgateway_spend_prompt_tokens_last_5m", help: "Prompt tokens in the last 5 minutes.", value: sumValue("prompt_tokens")},
	{name: "lmgateway_spend_completion_tokens_last_5m", help: "Completion tokens in the last 5 minutes.", value: sumValue("completion_tokens")},
	{name: "lmgateway_spend_reasoning_tokens_last_5m", help: "Reasoning tokens in the last 5 minutes.", value: sumValue("reasoning_tokens")},
	{name: "lmgateway_spend_cached_tokens_last_5m", help: "Cached tokens in the last 5 minutes.", value: sumValue("cached_tokens")},
	{name: "lmgateway_spend_total_tokens_last_5m", help: "Total tokens in the last 5 minutes.", value: sumValue("total_tokens")},
	{name: "lmgateway_spend_cost_usd_last_5m", help: "Spend cost in USD in the last 5 minutes.", value: sumValue("cost")},
	{name: "lmgateway_spend_latency_ms_sum_last_5m", help: "Total request latency in milliseconds in the last 5 minutes.", value: sumValue("latency_ms")},
	{name: "lmgateway_spend_latency_ms_avg_last_5m", help: "Average request latency in milliseconds in the last 5 minutes.", value: func(row store.TSAggRow) float64 {
		if row.Count == 0 {
			return 0
		}
		return row.Sums["latency_ms"] / float64(row.Count)
	}},
	{name: "lmgateway_spend_ttft_ms_sum_last_5m", help: "Total time to first token in milliseconds in the last 5 minutes.", value: sumValue("ttft_ms")},
	{name: "lmgateway_spend_ttft_ms_avg_last_5m", help: "Average time to first token in milliseconds in the last 5 minutes.", value: sampleAverage("ttft_ms", "ttft_samples")},
	{name: "lmgateway_spend_generation_ms_sum_last_5m", help: "Total generation time in milliseconds in the last 5 minutes.", value: sumValue("generation_ms")},
	{name: "lmgateway_spend_generation_ms_avg_last_5m", help: "Average generation time in milliseconds in the last 5 minutes.", value: sampleAverage("generation_ms", "generation_samples")},
	{name: "lmgateway_spend_generation_tokens_per_second_last_5m", help: "Completion tokens per generation second in the last 5 minutes.", value: func(row store.TSAggRow) float64 {
		seconds := row.Sums["generation_ms"] / 1000
		if seconds <= 0 {
			return 0
		}
		return row.Sums["generation_tokens"] / seconds
	}},
}

func sumValue(field string) func(store.TSAggRow) float64 {
	return func(row store.TSAggRow) float64 {
		return row.Sums[field]
	}
}

func sampleAverage(valueField, sampleField string) func(store.TSAggRow) float64 {
	return func(row store.TSAggRow) float64 {
		samples := row.Sums[sampleField]
		if samples <= 0 {
			return 0
		}
		return row.Sums[valueField] / samples
	}
}

func New(ts store.TSStore) http.Handler {
	return &Handler{ts: ts}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.ts == nil {
		http.Error(w, "metrics store is not configured", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.ts.Flush(ctx); err != nil {
		http.Error(w, "metrics flush failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	rows, err := h.ts.Aggregate(ctx, store.TSAgg{
		Stream:  "spend",
		From:    now.Add(-5 * time.Minute),
		To:      now,
		GroupBy: labelNames,
		Sum:     []string{"prompt_tokens", "completion_tokens", "reasoning_tokens", "cached_tokens", "total_tokens", "cost", "latency_ms", "ttft_ms", "ttft_samples", "generation_ms", "generation_samples", "generation_tokens"},
	})
	if err != nil {
		http.Error(w, "metrics query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, definition := range metricDefinitions {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", definition.name, definition.help, definition.name)
		for _, row := range rows {
			fmt.Fprintf(w, "%s{%s} %s\n", definition.name, formatLabels(row.Tags), formatValue(definition.value(row)))
		}
	}
}

func formatLabels(tags map[string]string) string {
	names := append([]string(nil), labelNames...)
	sort.Strings(names)
	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, name+"=\""+escapeLabel(tags[name])+"\"")
	}
	return strings.Join(values, ",")
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

func formatValue(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
