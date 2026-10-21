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

// The client-declared key_tag is a normal request-document field, so rules may
// Route hints are request-document fields, so rules may match them.
func TestRouteHintRuleMatchFieldAllowed(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x"}},
		Models:    []ModelCfg{{Name: "m", Provider: "p"}},
		Rules: []RuleCfg{{
			ID:    "hint",
			From:  "http",
			Match: []ConditionCfg{{Field: "route_hints.pool_policy", Op: "eq", Value: "price_first"}},
			Set:   map[string]any{"temperature": 0.5},
			To:    "http",
		}},
	}
	if _, err := Build(cfg, BuildDeps{}); err != nil {
		t.Fatalf("route hint rule should be accepted: %v", err)
	}
}

// The attribution tag is deliberately not a routing input.
func TestAttributionTagNotARoutingField(t *testing.T) {
	cfg := Config{
		Providers: []ProviderCfg{{Name: "p", Type: "openai", BaseURL: "http://x"}},
		Models:    []ModelCfg{{Name: "m", Provider: "p"}},
		Rules: []RuleCfg{{
			ID:    "tag",
			From:  "http",
			Match: []ConditionCfg{{Field: "key_tag", Op: "eq", Value: "cli"}},
			Set:   map[string]any{"temperature": 0.5},
			To:    "http",
		}},
	}
	if _, err := Build(cfg, BuildDeps{}); err == nil {
		t.Fatal("key_tag should not be accepted as a routing match field")
	}
}

// A pool precompiles one rule per ranking policy; the default rule is last.
func TestPoolPolicyRules(t *testing.T) {
	pools := pool.NewController()
	pools.Register(pool.Config{
		Model:   "smart",
		Backend: []string{"a/m", "b/m"},
		Policy:  pool.PolicySpeedFirst,
		Price:   map[string]float64{"a/m": 10, "b/m": 1},
	})
	pools.ObserveStats("smart", "a/m", 100, 10)
	pools.ObserveStats("smart", "b/m", 500, 1)

	cfg := Config{
		Providers: []ProviderCfg{
			{Name: "pa", Type: "openai", BaseURL: "http://a"},
			{Name: "pb", Type: "openai", BaseURL: "http://b"},
		},
		Models: []ModelCfg{
			{Name: "a/m", Provider: "pa", OutputPerMtok: 10},
			{Name: "b/m", Provider: "pb", OutputPerMtok: 1},
		},
		Pools: []PoolCfg{{Model: "smart", Backend: []string{"a/m", "b/m"}, Policy: pool.PolicySpeedFirst}},
	}
	rt, err := Build(cfg, BuildDeps{Pools: pools})
	if err != nil {
		t.Fatal(err)
	}

	var priceTarget, speedTarget, defaultTarget string
	for _, r := range rt.Table.All() {
		if r.From != "http" || len(r.Set) == 0 {
			continue
		}
		target, _ := r.Set["model"].(string)
		policy := ""
		for _, c := range r.Match.Conds {
			if c.Field != "route_hints.pool_policy" {
				continue
			}
			for _, value := range c.Value.([]any) {
				if value == pool.PolicyPriceFirst {
					policy = pool.PolicyPriceFirst
				}
				if value == pool.PolicySpeedFirst {
					policy = pool.PolicySpeedFirst
				}
			}
		}
		switch policy {
		case pool.PolicyPriceFirst:
			priceTarget = target
		case pool.PolicySpeedFirst:
			speedTarget = target
		default:
			if len(r.Match.Conds) == 1 && r.Match.Conds[0].Field == "model" {
				defaultTarget = target
			}
		}
	}
	if speedTarget != "a/m" || priceTarget != "b/m" || defaultTarget != "a/m" {
		t.Fatalf("policy targets: speed=%q price=%q default=%q", speedTarget, priceTarget, defaultTarget)
	}
}
