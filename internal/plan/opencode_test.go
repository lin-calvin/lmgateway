package plan

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func TestRefreshOpenCodeUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{
			"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T19:40:12Z"},
			"weekly":{"status":"ok","percent":20,"resetsAt":"2026-11-23T00:00:00Z"},
			"monthly":{"status":"ok","percent":30,"resetsAt":"2026-12-04T13:58:55Z"}
		}}`))
	}))
	defer upstream.Close()

	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()
	cfg := config.Config{
		Providers: []config.ProviderCfg{{
			Name:    "opencode_go",
			Type:    "openai",
			BaseURL: upstream.URL,
			APIKey:  "test-key",
			Plan:    &config.PlanCfg{Type: "opencode_go"},
		}},
	}
	m := config.NewManagerWithBase(mem.NewJSON(), ts, config.Config{})
	plans := New(m, ts)
	m.SetSourceRegistry(plans)
	if err := m.Build(cfg); err != nil {
		t.Fatal(err)
	}
	plans.pollTimers(context.Background())

	status := plans.Status()
	if len(status) != 1 || len(status[0].Snapshots) != 1 {
		t.Fatalf("status = %+v", status)
	}
	windows := status[0].Snapshots[0].Windows
	if len(windows) != 3 || windows[0].Name != "rolling" || windows[1].UsedPercent != 20 || windows[2].UsedPercent != 30 {
		t.Fatalf("windows = %+v", windows)
	}
}
