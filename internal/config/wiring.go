package config

import (
	"fmt"
	"sort"
	"strings"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/handler"
	"lmgateway/internal/match"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
	"lmgateway/internal/store"
	"lmgateway/internal/table"
)

// Wire priorities: lower matches first within a position. Bands make the
// ordering explicit and deterministic (map iteration no longer leaks in).
const (
	prioUser       = 0
	prioBuiltin    = 100
	prioModel      = 100
	prioDefault    = 200
	prioPrefix     = 300
	prioPoolPolicy = 350
	prioPool       = 400
)

// edge is a table rule plus the position it should occupy within its `from`.
type edge struct {
	rule     table.Rule
	priority int
	seq      int
}

// router accumulates edges in declaration order (seq) with a priority.
type router struct {
	edges []edge
	seq   int
}

func (b *router) Add(priority int, r table.Rule) {
	b.edges = append(b.edges, edge{rule: r, priority: priority, seq: b.seq})
	b.seq++
}

func sortedProviderNames(providers map[string]bool) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// compile assembles generated edges (with their priorities) and user edges into
// a table: a user edge on the same (from, match) replaces the generated edge,
// user edges sort first (priority 0), then a global respond fallback.
func compile(source *router, user []table.Rule) *table.Table {
	edges := append([]edge{}, dropOverridden(source.edges, user)...)
	for i, r := range user {
		edges = append(edges, edge{rule: r, priority: prioUser, seq: i})
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].priority != edges[j].priority {
			return edges[i].priority < edges[j].priority
		}
		return edges[i].seq < edges[j].seq
	})
	tbl := table.New()
	for _, e := range edges {
		tbl.Add(e.rule)
	}
	tbl.Add(table.Rule{Action: dispatch.ActionRespond, Generated: true}) // 全局 respond 兜底
	return tbl
}

// dropOverridden removes generated edges whose (from, match) a user edge shadows.
func dropOverridden(auto []edge, user []table.Rule) []edge {
	userKeys := map[string]bool{}
	for _, u := range user {
		userKeys[ruleKey(u)] = true
	}
	out := make([]edge, 0, len(auto))
	for _, a := range auto {
		if !userKeys[ruleKey(a.rule)] {
			out = append(out, a)
		}
	}
	return out
}

// ruleKey 规范化 (from + 排序后的条件) 作为身份
func ruleKey(r table.Rule) string {
	var conds []string
	for _, c := range r.Match.Conds {
		conds = append(conds, c.Field+"|"+string(c.Op)+"|"+fmt.Sprint(c.Value))
	}
	sort.Strings(conds)
	return r.From + ";" + strings.Join(conds, ";")
}

// wiring is the per-build environment components register themselves into. Each
// component contributes its data-plane action(s) and/or routing edges; the
// `actions`/`edges` gates split the two phases so routing-only recompiles reuse
// the (stateful) action registry without rebuilding providers.
type wiring struct {
	cfg     Config
	deps    BuildDeps
	ns      *namespace
	reg     *dispatch.Registry
	router  router
	env     providerEnv
	pricing map[string]handler.Pricing

	actions   bool
	edges     bool
	providers map[string]bool
}

func newWiring(cfg Config, deps BuildDeps, ns *namespace, reg *dispatch.Registry, actions, edges bool) *wiring {
	pricing := map[string]handler.Pricing{}
	upstream := map[string]string{}
	extraBody := map[string]map[string]any{}
	defaultExtraBody := map[string]map[string]any{}
	for _, m := range cfg.Models {
		if m.Name == "" || m.Provider == "" {
			continue
		}
		if m.UpstreamModel != "" {
			upstream[m.Name] = m.UpstreamModel
		}
		if len(m.ExtraBody) > 0 {
			extraBody[m.Name] = m.ExtraBody
		}
		if len(m.DefaultExtraBody) > 0 {
			defaultExtraBody[m.Name] = m.DefaultExtraBody
		}
		if m.InputPerMtok > 0 || m.OutputPerMtok > 0 {
			pricing[m.Name] = handler.Pricing{InputPerMtok: m.InputPerMtok, OutputPerMtok: m.OutputPerMtok}
		}
	}
	return &wiring{
		cfg: cfg, deps: deps, ns: ns, reg: reg, actions: actions, edges: edges,
		providers: map[string]bool{}, pricing: pricing,
		env: providerEnv{upstream: upstream, extraBody: extraBody, defaultExtraBody: defaultExtraBody, codex: deps.Codex},
	}
}

func (w *wiring) action(name string, fn any) {
	if w.actions {
		w.reg.Register(name, fn)
	}
}

func (w *wiring) provider(name string, fn any, discover dispatch.DiscoverFunc) {
	if w.actions {
		if discover != nil {
			w.reg.RegisterProvider(name, fn, discover)
		} else {
			w.reg.Register(name, fn)
		}
	}
	w.providers[name] = true
}

func (w *wiring) edge(priority int, r table.Rule) {
	if w.edges {
		w.router.Add(priority, r)
	}
}

// wireAll runs every component's self-registration.
func (w *wiring) wireAll() error {
	w.wirePipeline()
	if err := w.wireProviders(); err != nil {
		return err
	}
	if err := w.wireModels(); err != nil {
		return err
	}
	return w.wirePools()
}

// wirePipeline is the builtin pipeline component: it registers the ingress/response
// handlers and their edges (@ingress → observe → [authorize] → [ratelimit] → http,
// usage → logprobs, stream → stream).
func (w *wiring) wirePipeline() {
	withPools := w.deps.Pools != nil && len(w.cfg.Pools) > 0

	if w.actions {
		var ts store.TSStore
		if w.deps.Storage != nil {
			ts = w.deps.Storage.TS
		}
		w.reg.Register("stream", handler.Stream{}.Handle)
		w.reg.Register("observe", handler.Observe{}.Handle)
		if !w.ns.empty() {
			resolve := func(model string) ([]string, []string, bool) {
				e, ok := w.ns.resolve(model)
				return e.Allowed, e.Denied, ok
			}
			known := func(model string) bool { return w.ns.knownName(model) }
			w.reg.Register("authorize", handler.NewAuthorizer(resolve, known).Handle)
		}
		if withPools {
			w.reg.Register("ratelimit", handler.NewRatelimit(w.deps.Pools).Handle)
		}
		if w.deps.UsageHooks != nil {
			w.reg.Register("usage", handler.UsageCollector{Hooks: w.deps.UsageHooks}.Handle)
		} else {
			w.reg.Register("usage", handler.NewSpendRecorder(ts, w.pricing).Handle)
		}
		w.reg.Register("logprobs", handler.NewLogprobsExporter(handler.LogprobsConfig{
			WebhookURL: w.cfg.Logprobs.WebhookURL,
			TimeoutSec: w.cfg.Logprobs.TimeoutSec,
		}).Handle)
	}

	afterObserve := packet.SourceHTTP
	if withPools {
		afterObserve = "ratelimit"
		w.edge(prioBuiltin, table.Rule{From: "ratelimit", Action: "ratelimit", To: packet.SourceHTTP, Generated: true})
	}
	if w.reg.Has("authorize") {
		w.edge(prioBuiltin, table.Rule{From: packet.SourceIngress, Action: "observe", To: "authorize", Generated: true})
		w.edge(prioBuiltin, table.Rule{From: "authorize", Action: "authorize", To: afterObserve, Generated: true})
	} else {
		w.edge(prioBuiltin, table.Rule{From: packet.SourceIngress, Action: "observe", To: afterObserve, Generated: true})
	}
	w.edge(prioBuiltin, table.Rule{From: "usage", Action: "logprobs", Generated: true})
	w.edge(prioBuiltin, table.Rule{From: "stream", Action: "stream", Generated: true})
}

// wireProviders is the provider component: each provider registers its own
// action (via the type factory) and its routing edges.
func (w *wiring) wireProviders() error {
	for _, p := range w.cfg.Providers {
		if p.Name == "" {
			continue
		}
		if w.actions {
			factory, ok := providerFactories[p.Type]
			if !ok {
				return fmt.Errorf("provider %s: unknown type %q", p.Name, p.Type)
			}
			h, discover, err := factory(p, w.env)
			if err != nil {
				return err
			}
			if !p.Discover {
				discover = nil
			}
			w.provider(p.Name, h, discover)
		}
		w.providers[p.Name] = true
	}
	for _, name := range sortedProviderNames(w.providers) {
		w.edge(prioPrefix, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpPrefix, Value: name + "/"}),
			Action:    name,
			Generated: true,
		})
	}
	for _, name := range sortedProviderNames(w.providers) {
		w.edge(prioBuiltin, table.Rule{From: name, Action: "usage", Generated: true})
	}
	return nil
}

// wireModels is the model component: http [model==name] → provider, plus the
// default-model fallback.
func (w *wiring) wireModels() error {
	defaultProvider, defaultModel := "", ""
	for _, m := range w.cfg.Models {
		if m.Default && m.Provider != "" {
			defaultProvider = m.Provider
			defaultModel = m.Name
		}
	}
	for _, m := range w.cfg.Models {
		if m.Name == "" {
			continue
		}
		if !w.providers[m.Provider] {
			return fmt.Errorf("model %s: references unknown provider %q", m.Name, m.Provider)
		}
		w.edge(prioModel, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpEq, Value: m.Name}),
			Action:    m.Provider,
			Generated: true,
		})
	}
	if defaultProvider != "" && w.providers[defaultProvider] {
		w.edge(prioDefault, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpEmpty}),
			Set:       map[string]any{"model": defaultModel},
			Action:    defaultProvider,
			Generated: true,
		})
	}
	return nil
}

// wirePools is the pool component: it validates pools, registers them with the
// controller, and wires the alias → active-backend pure-transform edge.
func (w *wiring) wirePools() error {
	if len(w.cfg.Pools) == 0 {
		return nil
	}
	poolsCtrl := w.deps.Pools
	if poolsCtrl == nil {
		return fmt.Errorf("pools configured but no pool controller was provided")
	}
	modelNames := map[string]bool{}
	priceOf := map[string]float64{}
	for _, m := range w.cfg.Models {
		modelNames[m.Name] = true
		switch {
		case m.OutputPerMtok > 0:
			priceOf[m.Name] = m.OutputPerMtok
		case m.InputPerMtok > 0:
			priceOf[m.Name] = m.InputPerMtok
		}
	}
	poolNames := map[string]bool{}
	for _, p := range w.cfg.Pools {
		poolNames[p.Model] = true
	}
	for _, p := range w.cfg.Pools {
		if p.Model == "" || len(p.Backend) == 0 {
			return fmt.Errorf("pool: model and backend are required")
		}
		if modelNames[p.Model] {
			return fmt.Errorf("pool %s: name collides with a model", p.Model)
		}
		if p.Fallback == p.Model {
			return fmt.Errorf("pool %s: fallback must differ from the pool model", p.Model)
		}
		for _, backend := range p.Backend {
			if backend == p.Model {
				return fmt.Errorf("pool %s: a backend must differ from the pool model", p.Model)
			}
		}
		if w.actions {
			poolsCtrl.Register(pool.Config{
				Model:          p.Model,
				Backend:        p.Backend,
				CooldownSec:    p.CooldownSec,
				OnAllLimited:   p.OnAllLimited,
				Policy:         p.Policy,
				ReselectTTFTMs: poolReselectTTFTMs(p),
				Price:          priceOf,
				Fallback:       p.Fallback,
			})
		}
		// Precompile one rule per ranking policy. This also runs in routing-only
		// rebuilds (actions=false), where the pool was registered by the previous
		// full build. A missing/unknown policy hint falls through to the default
		// pool rule below.
		for _, policy := range pool.KnownPolicies {
			target, ok := poolsCtrl.ActiveForPolicy(p.Model, policy)
			if !ok || target == "" {
				continue
			}
			w.edge(prioPoolPolicy, table.Rule{
				From: "http",
				Match: match.All(
					match.Condition{Field: "model", Op: match.OpEq, Value: p.Model},
					match.Condition{Field: "route_hints.pool_policy", Op: match.OpIn, Value: policyAliases(policy)},
				),
				Set:       map[string]any{"model": target},
				To:        "http",
				Generated: true,
			})
		}
		active, ok := poolsCtrl.Active(p.Model)
		if !ok {
			active = p.Backend[0]
		}
		w.edge(prioPool, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpEq, Value: p.Model}),
			Set:       map[string]any{"model": active},
			To:        "http",
			Generated: true,
		})
	}
	// reject fallback cycles among pool targets (model fallbacks can't cycle)
	for _, p := range w.cfg.Pools {
		if !poolNames[p.Fallback] {
			continue
		}
		seen := map[string]bool{p.Model: true}
		for next := p.Fallback; poolNames[next]; {
			if seen[next] {
				return fmt.Errorf("pool %s: fallback cycle via %s", p.Model, next)
			}
			seen[next] = true
			nb := ""
			for _, q := range w.cfg.Pools {
				if q.Model == next {
					nb = q.Fallback
					break
				}
			}
			next = nb
		}
	}
	return nil
}

// policyAliases are the accepted values of route_hints.pool_policy for one strategy.
func policyAliases(policy string) []any {
	switch policy {
	case pool.PolicyPriceFirst:
		return []any{policy, "price"}
	case pool.PolicySpeedFirst:
		return []any{policy, "speed"}
	default:
		return []any{policy}
	}
}
