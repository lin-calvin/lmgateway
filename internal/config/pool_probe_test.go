package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func streamHandler(delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []map[string]any{
			{"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": nil}}},
			{"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}},
		}
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
}

func TestProbePipelineUpdatesStatsAndReselects(t *testing.T) {
	fast := httptest.NewServer(streamHandler(0))
	defer fast.Close()
	slow := httptest.NewServer(streamHandler(150 * time.Millisecond))
	defer slow.Close()

	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	seed := Config{
		Providers: []ProviderCfg{
			{Name: "fast", Type: "openai", BaseURL: fast.URL},
			{Name: "slow", Type: "openai", BaseURL: slow.URL},
		},
		Models: []ModelCfg{
			{Name: "fast/m", Provider: "fast"},
			{Name: "slow/m", Provider: "slow"},
		},
		Pools: []PoolCfg{{
			Model:            "pooled",
			Backend:          []string{"slow/m", "fast/m"},
			Policy:           "speed_first",
			ReselectTTFTMs:   5,
			ProbeIntervalMin: 1,
			CooldownSec:      60,
		}},
	}
	m := NewManagerWithBase(js, ts, seed)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	rt := m.Runtime()
	if before, _ := m.pools.Status("pooled"); before.Active != "slow/m" {
		t.Fatalf("expected initial active slow/m, got %q", before.Active)
	}
	m.runDueProbes(rt, map[string]time.Time{})
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	m.refreshPoolStats(ctx, rt)

	status, _ := m.pools.Status("pooled")
	slowStat, fastStat := status.Stats["slow/m"], status.Stats["fast/m"]
	if slowStat.Samples == 0 || fastStat.Samples == 0 {
		t.Fatalf("probe stats not recorded: %+v", status.Stats)
	}
	if slowStat.TTFTMs <= fastStat.TTFTMs {
		t.Fatalf("slow backend should have higher ttft: slow=%.1f fast=%.1f", slowStat.TTFTMs, fastStat.TTFTMs)
	}
	if status.Active != "fast/m" {
		t.Fatalf("expected reselect to fast/m, got %q", status.Active)
	}
}
