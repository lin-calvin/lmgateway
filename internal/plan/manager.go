// Package plan stores the latest upstream rate-limit snapshots and computes
// plan-level cost. It is a long-lived object shared by the API, pool and web
// console; provider adapters never own plan state.
package plan

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"lmgateway/internal/codingplan"
	"lmgateway/internal/config"
	"lmgateway/internal/ratelimit"
	"lmgateway/internal/store"
)

// Manager is the plan/rate-limit registry.
type Manager struct {
	mu        sync.Mutex
	snapshots map[string]ratelimit.Snapshot // key: provider + "/" + limit_id
	sources   map[string]codingplan.Source
	cfg       *config.Manager
	ts        store.TSStore
}

func New(cfg *config.Manager, ts store.TSStore) *Manager {
	return &Manager{
		snapshots: map[string]ratelimit.Snapshot{},
		sources:   map[string]codingplan.Source{},
		cfg:       cfg,
		ts:        ts,
	}
}

// ReplaceSources swaps the provider plan capabilities. It is called after a
// successful config build/reload so timer closures use the current API keys and
// removed providers disappear.
func (m *Manager) ReplaceSources(sources map[string]codingplan.Source) {
	copy := make(map[string]codingplan.Source, len(sources))
	for name, source := range sources {
		copy[name] = source
	}
	m.mu.Lock()
	m.sources = copy
	m.mu.Unlock()
}

// Start runs the control-plane timers (currently OpenCode Go usage). Sink
// sources are driven by Observe on the response path.
func (m *Manager) Start(ctx context.Context) {
	go func() {
		m.pollTimers(ctx)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.pollTimers(ctx)
			}
		}
	}()
}

func (m *Manager) pollTimers(ctx context.Context) {
	m.mu.Lock()
	sources := make(map[string]codingplan.Source, len(m.sources))
	for name, source := range m.sources {
		sources[name] = source
	}
	m.mu.Unlock()
	for provider, source := range sources {
		if source.Timer == nil {
			continue
		}
		snap, err := source.Timer(ctx)
		if err != nil || snap == nil {
			continue
		}
		m.storeSnapshot(provider, *snap)
	}
}

func (m *Manager) storeSnapshot(provider string, snap ratelimit.Snapshot) {
	if snap.ObservedAt.IsZero() {
		snap.ObservedAt = time.Now().UTC()
	}
	snap.Provider = provider
	limit := snap.LimitID
	if limit == "" {
		limit = "default"
	}
	m.mu.Lock()
	if m.snapshots == nil {
		m.snapshots = map[string]ratelimit.Snapshot{}
	}
	m.snapshots[provider+"/"+limit] = snap
	m.mu.Unlock()
}

// Observe consumes a rate-limit event from the usage collector. A provider
// sink (Codex, generic OpenAI headers) runs first; otherwise the manager falls
// back to the generic snapshot/header parsers.
func (m *Manager) Observe(ev ratelimit.Event) {
	if m == nil {
		return
	}
	m.mu.Lock()
	source := m.sources[ev.Provider]
	m.mu.Unlock()
	if source.Sink != nil {
		if snap := source.Sink(ev); snap != nil {
			m.storeSnapshot(ev.Provider, *snap)
			return
		}
	}
	if m.cfg == nil {
		return
	}
	rt := m.cfg.Runtime()
	if rt == nil {
		return
	}
	provider := ev.Provider
	var plan *config.PlanCfg
	for i := range rt.Config.Providers {
		if rt.Config.Providers[i].Name == provider {
			plan = rt.Config.Providers[i].Plan
			break
		}
	}

	snapshots := []ratelimit.Snapshot{}
	if len(ev.Headers) > 0 {
		family := "generic"
		if plan != nil && plan.RateLimitFamily != "" {
			family = plan.RateLimitFamily
		} else {
			for _, p := range rt.Config.Providers {
				if p.Name == provider && p.Type == "chatgpt_codex" {
					family = "codex"
				}
			}
		}
		if family == "codex" {
			snapshots = append(snapshots, ratelimit.ParseCodexHeaders(ev.Headers)...)
		} else if snap := ratelimit.ParseGeneric(ev.Headers); snap != nil {
			snapshots = append(snapshots, *snap)
		}
	}
	// A parsed snapshot (Codex SSE capture) is newer than the response headers;
	// override the same limit id, or add it when headers had no windows.
	if ev.Snapshot != nil {
		snap := *ev.Snapshot
		replaced := false
		for i := range snapshots {
			if snapshots[i].LimitID == snap.LimitID {
				snapshots[i] = snap
				replaced = true
				break
			}
		}
		if !replaced {
			snapshots = append(snapshots, snap)
		}
	}
	for _, snap := range snapshots {
		m.storeSnapshot(provider, snap)
	}
}

// Status is one plan's API view.
type Status struct {
	Provider        string                           `json:"provider"`
	Type            string                           `json:"type,omitempty"`
	Price           float64                          `json:"price,omitempty"`
	Currency        string                           `json:"currency,omitempty"`
	Period          string                           `json:"period,omitempty"`
	CostModel       string                           `json:"cost_model,omitempty"`
	Multipliers     map[string]float64               `json:"multipliers,omitempty"`
	ModelPrices     map[string]config.PlanModelPrice `json:"model_prices,omitempty"`
	Window          string                           `json:"window,omitempty"`
	RateLimitFamily string                           `json:"rate_limit_family,omitempty"`
	Snapshots       []ratelimit.Snapshot             `json:"snapshots,omitempty"`
	Credits         *ratelimit.Credits               `json:"credits,omitempty"`
	Cost            *Cost                            `json:"cost,omitempty"`
	ObservedAt      time.Time                        `json:"observed_at,omitempty"`
}

// Cost is a plan-level cost view.
type Cost struct {
	Mode   string  `json:"mode"`
	Today  float64 `json:"today,omitempty"`
	Month  float64 `json:"month,omitempty"`
	Value  float64 `json:"value,omitempty"`
	Window string  `json:"window,omitempty"`
}

// Status returns one entry per provider with a plan.
func (m *Manager) Status() []Status {
	if m == nil || m.cfg == nil {
		return nil
	}
	rt := m.cfg.Runtime()
	if rt == nil {
		return nil
	}
	out := []Status{}
	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, provider := range rt.Config.Providers {
		if provider.Plan == nil {
			continue
		}
		p := provider.Plan
		st := Status{
			Provider:        provider.Name,
			Type:            p.Type,
			Price:           p.Price,
			Currency:        p.Currency,
			Period:          p.Period,
			CostModel:       p.CostModel,
			Multipliers:     p.Multipliers,
			ModelPrices:     p.ModelPrices,
			Window:          p.Window,
			RateLimitFamily: p.RateLimitFamily,
		}
		m.mu.Lock()
		for key, snap := range m.snapshots {
			if strings.HasPrefix(key, provider.Name+"/") {
				st.Snapshots = append(st.Snapshots, snap)
				if st.ObservedAt.IsZero() || snap.ObservedAt.After(st.ObservedAt) {
					st.ObservedAt = snap.ObservedAt
				}
				if snap.Credits != nil {
					st.Credits = snap.Credits
				}
			}
		}
		m.mu.Unlock()
		sort.Slice(st.Snapshots, func(i, j int) bool { return st.Snapshots[i].LimitID < st.Snapshots[j].LimitID })

		switch p.CostModel {
		case "window_share":
			window := p.Window
			if window == "" {
				window = "secondary"
			}
			if used, ok := windowUsed(st.Snapshots, window); ok {
				st.Cost = &Cost{Mode: "window_share", Window: window, Value: p.Price * used / 100, Month: p.Price * used / 100}
			}
		case "multiplier", "":
			// Cost records are tagged with the provider (the plan key), not the
			// preset type, so two accounts using the same preset stay separate.
			today := m.sumPlanCost(provider.Name, dayStart, now)
			month := m.sumPlanCost(provider.Name, monthStart, now)
			st.Cost = &Cost{Mode: "multiplier", Today: today, Month: month}
		}
		out = append(out, st)
	}
	return out
}

func windowUsed(snapshots []ratelimit.Snapshot, name string) (float64, bool) {
	for _, snap := range snapshots {
		for _, window := range snap.Windows {
			if strings.EqualFold(window.Name, name) {
				return window.UsedPercent, true
			}
		}
	}
	return 0, false
}

func (m *Manager) sumPlanCost(planName string, from, to time.Time) float64 {
	if m.ts == nil {
		return 0
	}
	recs, err := m.ts.Query(context.Background(), store.TSQuery{
		Stream: "spend",
		From:   from,
		To:     to,
		Tag:    map[string]string{"plan": planName},
	})
	if err != nil {
		return 0
	}
	sum := 0.0
	for _, rec := range recs {
		sum += fieldFloat(rec.Fields["plan_cost"])
	}
	return sum
}

func fieldFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
