package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/pool"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

// A backend dead from a quota error is revived once its provider's balance API
// reports the account is funded again.
func TestRecoverBalancesRevivesQuotaDead(t *testing.T) {
	funded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if funded {
			w.Write([]byte(`{"data":{"assets":10}}`))
		} else {
			w.Write([]byte(`{"data":{"assets":0}}`))
		}
	}))
	defer srv.Close()

	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	seed := Config{
		Providers: []ProviderCfg{{
			Name: "p", Type: "openai", BaseURL: "http://x",
			BalanceType: "custom", BalancePath: srv.URL, BalanceJSONPath: "data.assets",
		}},
		Models: []ModelCfg{{Name: "m", Provider: "p"}},
		Pools:  []PoolCfg{{Model: "pooled", Backend: []string{"m"}}},
	}
	m := NewManagerWithBase(js, ts, seed)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Mark the backend dead due to quota.
	m.Pools().Report("pooled", "m", "quota", 0)
	if got := m.Pools(); got != nil {
		if st, ok := got.Status("pooled"); !ok || st.Health["m"].Health != string(pool.HealthDead) {
			t.Fatalf("expected backend dead, got %+v", st.Health)
		}
	}

	// Still unfunded: sets the interval stamp, stays dead.
	last := map[string]time.Time{}
	m.Pools().RecoverBalances(context.Background(), balanceResolver(m.Runtime().Config), last)
	if st, _ := m.Pools().Status("pooled"); st.Health["m"].Health != string(pool.HealthDead) {
		t.Fatalf("unfunded account should stay dead, got %+v", st.Health["m"])
	}

	// Funded, but within the interval → still gated.
	funded = true
	m.Pools().RecoverBalances(context.Background(), balanceResolver(m.Runtime().Config), last)
	if st, _ := m.Pools().Status("pooled"); st.Health["m"].Health != string(pool.HealthDead) {
		t.Fatalf("interval gating should suppress the re-check, got %+v", st.Health["m"])
	}

	// Interval elapsed → revived.
	m.Pools().RecoverBalances(context.Background(), balanceResolver(m.Runtime().Config), map[string]time.Time{})
	st, _ := m.Pools().Status("pooled")
	if st.Health["m"].Health == string(pool.HealthDead) {
		t.Fatalf("funded account should be revived, got %+v", st.Health["m"])
	}
}

// A backend dead for a non-quota reason must not be touched by balance recovery.
func TestRecoverBalancesIgnoresNonQuota(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"data":{"assets":10}}`))
	}))
	defer srv.Close()

	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	seed := Config{
		Providers: []ProviderCfg{{
			Name: "p", Type: "openai", BaseURL: "http://x",
			BalanceType: "custom", BalancePath: srv.URL, BalanceJSONPath: "data.assets",
		}},
		Models: []ModelCfg{{Name: "m", Provider: "p"}},
		Pools:  []PoolCfg{{Model: "pooled", Backend: []string{"m"}}},
	}
	m := NewManagerWithBase(js, ts, seed)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	m.Pools().Report("pooled", "m", "auth", 0) // dead, but reason=auth
	m.Pools().RecoverBalances(context.Background(), balanceResolver(m.Runtime().Config), map[string]time.Time{})
	if called {
		t.Fatal("balance endpoint must not be queried for a non-quota dead backend")
	}
	if st, _ := m.Pools().Status("pooled"); st.Health["m"].Health != string(pool.HealthDead) {
		t.Fatalf("auth-dead backend should stay dead, got %+v", st.Health["m"])
	}
}
