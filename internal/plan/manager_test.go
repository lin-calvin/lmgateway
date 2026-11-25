package plan_test

import (
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/plan"
	"lmgateway/internal/planstate"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func testManager(t *testing.T, cfg config.Config) (*config.Manager, store.TSStore) {
	t.Helper()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	m := config.NewManagerWithBase(mem.NewJSON(), ts, config.Config{})
	if err := m.Build(cfg); err != nil {
		t.Fatal(err)
	}
	return m, ts
}

func TestObserveCodexHeadersAndWindowShare(t *testing.T) {
	m, ts := testManager(t, config.Config{
		Providers: []config.ProviderCfg{{
			Name:    "codex",
			Type:    "openai",
			BaseURL: "http://x",
			Plan:    &config.PlanCfg{Type: "chatgpt_plus"},
		}},
		Models: []config.ModelCfg{{Name: "gpt-5.6", Provider: "codex"}},
	})
	plans := plan.New(m, ts)
	plans.Observe(planstate.Event{
		Provider: "codex",
		Headers: map[string]string{
			"x-codex-primary-used-percent":   "10",
			"x-codex-secondary-used-percent": "25",
			"x-codex-plan-type":              "plus",
		},
		At: time.Now().UTC(),
	})

	status := plans.Status()
	if len(status) != 1 {
		t.Fatalf("status = %+v", status)
	}
	got := status[0]
	if got.Provider != "codex" || got.Type != "chatgpt_plus" || got.Price != 20 {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Snapshots) != 1 || len(got.Snapshots[0].Windows) != 2 {
		t.Fatalf("snapshots = %+v", got.Snapshots)
	}
	if got.Cost == nil || got.Cost.Mode != "window_share" || got.Cost.Value != 5 {
		t.Fatalf("cost = %+v", got.Cost)
	}
}

func TestObserveGenericAndMultiplier(t *testing.T) {
	m, ts := testManager(t, config.Config{
		Providers: []config.ProviderCfg{{
			Name:    "opencode_go",
			Type:    "openai",
			BaseURL: "http://x",
			Plan:    &config.PlanCfg{Type: "opencode_go"},
		}},
		Models: []config.ModelCfg{{Name: "gpt-5.6", Provider: "opencode_go"}},
	})
	plans := plan.New(m, ts)
	plans.Observe(planstate.Event{
		Provider: "opencode_go",
		Headers: map[string]string{
			"ratelimit-limit":     "100",
			"ratelimit-remaining": "20",
		},
		At: time.Now().UTC(),
	})

	status := plans.Status()
	if len(status) != 1 {
		t.Fatalf("status = %+v", status)
	}
	got := status[0]
	if got.Type != "opencode_go" || got.CostModel != "multiplier" {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].Windows[0].UsedPercent != 80 {
		t.Fatalf("snapshots = %+v", got.Snapshots)
	}
	if got.Cost == nil || got.Cost.Mode != "multiplier" {
		t.Fatalf("cost = %+v", got.Cost)
	}
}
