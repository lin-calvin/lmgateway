package config

import (
	"testing"

	"lmgateway/internal/match"
	"lmgateway/internal/pool"
)

// Build must produce a deterministic table independent of map iteration order.
func TestBuildTableDeterministicOrder(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{
			{Name: "zeta", Type: "openai", BaseURL: "http://z"},
			{Name: "alpha", Type: "openai", BaseURL: "http://a"},
			{Name: "mid", Type: "openai", BaseURL: "http://m"},
		},
		Models: []ModelCfg{{Name: "m", Provider: "alpha"}},
	}
	snapshot := func() []string {
		rt, err := Build(cfg, BuildDeps{})
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range rt.Table.All() {
			out = append(out, r.From+"->"+r.Action+"->"+r.To)
		}
		return out
	}
	a, b := snapshot(), snapshot()
	if len(a) != len(b) {
		t.Fatalf("length differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("nondeterministic at %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// Within the http position, model edges must sort before prefix edges before the
// pool transform edge (priority bands).
func TestBuildTableHTTPPriority(t *testing.T) {
	pools := pool.NewController()
	cfg := Config{
		Providers: []ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x"}},
		Models:    []ModelCfg{{Name: "m", Provider: "p"}},
		Pools:     []PoolCfg{{Model: "pooled", Backend: []string{"m"}}},
	}
	rt, err := Build(cfg, BuildDeps{Pools: pools})
	if err != nil {
		t.Fatal(err)
	}
	kind := func(r ruleView) string {
		if len(r.Set) > 0 {
			return "pool"
		}
		for _, c := range r.Conds {
			if c.Op == match.OpPrefix {
				return "prefix"
			}
			if c.Op == match.OpEq {
				return "model"
			}
		}
		return "other"
	}
	var order []string
	for _, r := range rt.Table.All() {
		if r.From != "http" {
			continue
		}
		order = append(order, kind(ruleView{Set: r.Set, Conds: r.Match.Conds}))
	}
	// model (m) → prefix (p/) → pool (pooled)
	if len(order) < 3 || order[0] != "model" || order[1] != "prefix" || order[2] != "pool" {
		t.Fatalf("http edge order = %v, want [model prefix pool ...]", order)
	}
}

type ruleView struct {
	Set   map[string]any
	Conds []match.Condition
}
