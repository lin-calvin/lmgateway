package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestMetricsLastFiveMinutes(t *testing.T) {
	ctx := context.Background()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	for _, record := range []*store.TSRecord{
		{
			Ts:     time.Now().UTC().Add(-time.Minute),
			Stream: "spend",
			Tags:   map[string]string{"provider": "chatgpt_codex", "model": "gpt-5.6-luna", "stream": "true", "status": "ok"},
			Fields: map[string]any{"prompt_tokens": 20.0, "completion_tokens": 5.0, "reasoning_tokens": 2.0, "cached_tokens": 10.0, "total_tokens": 25.0, "cost": 0.1, "latency_ms": 100.0, "ttft_ms": 20.0, "ttft_samples": 1.0, "generation_ms": 250.0, "generation_samples": 1.0, "generation_tokens": 5.0},
		},
		{
			Ts:     time.Now().UTC().Add(-2 * time.Minute),
			Stream: "spend",
			Tags:   map[string]string{"provider": "chatgpt_codex", "model": "gpt-5.6-luna", "stream": "true", "status": "ok"},
			Fields: map[string]any{"prompt_tokens": 10.0, "completion_tokens": 3.0, "reasoning_tokens": 1.0, "cached_tokens": 4.0, "total_tokens": 13.0, "cost": 0.2, "latency_ms": 50.0, "ttft_ms": 40.0, "ttft_samples": 1.0, "generation_ms": 150.0, "generation_samples": 1.0, "generation_tokens": 3.0},
		},
	} {
		if err := ts.Append(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	New(ts).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("unexpected content type: %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	labels := `model="gpt-5.6-luna",provider="chatgpt_codex",status="ok",stream="true"`
	checks := []string{
		"lmgateway_spend_requests_last_5m{" + labels + `} 2`,
		"lmgateway_spend_prompt_tokens_last_5m{" + labels + `} 30`,
		"lmgateway_spend_completion_tokens_last_5m{" + labels + `} 8`,
		"lmgateway_spend_reasoning_tokens_last_5m{" + labels + `} 3`,
		"lmgateway_spend_cached_tokens_last_5m{" + labels + `} 14`,
		"lmgateway_spend_total_tokens_last_5m{" + labels + `} 38`,
		"lmgateway_spend_cost_usd_last_5m{" + labels + `} 0.3`,
		"lmgateway_spend_latency_ms_sum_last_5m{" + labels + `} 150`,
		"lmgateway_spend_latency_ms_avg_last_5m{" + labels + `} 75`,
		"lmgateway_spend_ttft_ms_sum_last_5m{" + labels + `} 60`,
		"lmgateway_spend_ttft_ms_avg_last_5m{" + labels + `} 30`,
		"lmgateway_spend_generation_ms_sum_last_5m{" + labels + `} 400`,
		"lmgateway_spend_generation_ms_avg_last_5m{" + labels + `} 200`,
		"lmgateway_spend_generation_tokens_per_second_last_5m{" + labels + `} 20`,
	}
	for _, check := range checks {
		if !strings.Contains(body, check) {
			t.Errorf("metrics missing %q in:\n%s", check, body)
		}
	}
}

func TestMetricsMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rec := httptest.NewRecorder()
	New(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("unexpected Allow header: %q", rec.Header().Get("Allow"))
	}
}
