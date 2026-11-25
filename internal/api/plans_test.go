package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/api"
	"lmgateway/internal/config"
	"lmgateway/internal/plan"
	"lmgateway/internal/planstate"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestPlansStatusAndPresets(t *testing.T) {
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()
	js := mem.NewJSON()
	cfg := config.Config{
		Providers: []config.ProviderCfg{{
			Name:    "opencode_go",
			Type:    "openai",
			BaseURL: "http://x",
			Plan:    &config.PlanCfg{Type: "opencode_go"},
		}},
		Models: []config.ModelCfg{{Name: "gpt-5.6", Provider: "opencode_go"}},
	}
	m := config.NewManagerWithBase(js, ts, config.Config{})
	if err := m.Build(cfg); err != nil {
		t.Fatal(err)
	}
	plans := plan.New(m, ts)
	plans.Observe(planstate.Event{
		Provider: "opencode_go",
		Headers:  map[string]string{"ratelimit-limit": "100", "ratelimit-remaining": "25"},
		At:       time.Now().UTC(),
	})

	srv := httptest.NewServer(api.New(api.Deps{Manager: m, TS: ts, Rollup: rollup.New(ts, js), Plans: plans}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/plans/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		Plans []plan.Status `json:"plans"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.Plans) != 1 || status.Plans[0].Provider != "opencode_go" || status.Plans[0].Cost == nil {
		t.Fatalf("status = %+v", status.Plans)
	}

	resp2, err := http.Get(srv.URL + "/api/plans/presets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var presets struct {
		Presets map[string]config.PlanPreset `json:"presets"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&presets); err != nil {
		t.Fatal(err)
	}
	if presets.Presets["opencode_go"].Price != 10 {
		t.Fatalf("presets = %+v", presets.Presets)
	}
}
