// Package pool implements the control-plane side of backend pools: a pool maps
// one logical model (alias) to an ordered list of backend model ids.
//
// Selection is a multi-key sort with a sticky gate:
//
//	keys: cooling (penalty) ▸ price ▸ ttft  (order chosen by policy)
//
// The active backend stays put until it is cooling, or its probe-derived ttft
// exceeds reselect_ttft_ms. There is deliberately no round-robin: switching
// costs provider-side prefix/KV cache.
package pool

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Policy names the sort key order.
const (
	PolicySpeedFirst = "speed_first" // ttft primary, price tiebreak
	PolicyPriceFirst = "price_first" // price primary, ttft tiebreak
)

// Config registers one pool.
type Config struct {
	Model          string             // alias clients call
	Backend        []string           // ordered backend model ids (declaration order = final tiebreak)
	CooldownSec    int                // fallback cooldown when the upstream sends no retry-after
	OnAllLimited   string             // "fail" (default) or "force-least-recent"
	Policy         string             // speed_first (default) or price_first
	ReselectTTFTMs int                // absolute trigger: switch when active ttft exceeds this
	Price          map[string]float64 // backend model id → price (0 for subscription plans)
}

// Controller holds all pool state. It is safe for concurrent use.
type Controller struct {
	mu     sync.Mutex
	pools  map[string]*state
	notify func() // called (unlocked) when the active backend changes
}

type state struct {
	backends       []string
	active         string
	cooldown       map[string]time.Time
	cooldownDur    time.Duration
	onAllLimited   string
	policy         string
	reselectTTFTMs int
	price          map[string]float64
	stats          map[string]*backendStat
}

// backendStat is the exponentially-weighted observation of one backend.
type backendStat struct {
	ttftMs  float64
	cost    float64
	samples int
	at      time.Time
}

const statAlpha = 0.35 // EWMA weight for the newest sample

func NewController() *Controller {
	return &Controller{pools: map[string]*state{}}
}

// SetNotifier registers a callback invoked whenever a rotation changes the
// active backend. The manager uses it to schedule an in-memory routing recompile.
func (c *Controller) SetNotifier(fn func()) {
	c.mu.Lock()
	c.notify = fn
	c.mu.Unlock()
}

// Register installs/replaces a pool. Empty/invalid configs are ignored. Runtime
// health and stats are preserved across hot reloads.
func (c *Controller) Register(cfg Config) {
	if cfg.Model == "" || len(cfg.Backend) == 0 {
		return
	}
	cooldown := time.Duration(cfg.CooldownSec) * time.Second
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	onAll := cfg.OnAllLimited
	if onAll == "" {
		onAll = "fail"
	}
	policy := cfg.Policy
	if policy != PolicyPriceFirst {
		policy = PolicySpeedFirst
	}
	price := make(map[string]float64, len(cfg.Price))
	for k, v := range cfg.Price {
		price[k] = v
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.pools[cfg.Model]; ok {
		existing.backends = append([]string(nil), cfg.Backend...)
		existing.cooldownDur = cooldown
		existing.onAllLimited = onAll
		existing.policy = policy
		existing.reselectTTFTMs = cfg.ReselectTTFTMs
		existing.price = price
		if !existing.has(existing.active) {
			existing.active = cfg.Backend[0]
		}
		for backend := range existing.cooldown {
			if !existing.has(backend) {
				delete(existing.cooldown, backend)
			}
		}
		for backend := range existing.stats {
			if !existing.has(backend) {
				delete(existing.stats, backend)
			}
		}
		return
	}
	c.pools[cfg.Model] = &state{
		backends:       append([]string(nil), cfg.Backend...),
		active:         cfg.Backend[0],
		cooldown:       map[string]time.Time{},
		cooldownDur:    cooldown,
		onAllLimited:   onAll,
		policy:         policy,
		reselectTTFTMs: cfg.ReselectTTFTMs,
		price:          price,
		stats:          map[string]*backendStat{},
	}
}

// Active returns the backend to serve the next request, applying the sort and
// sticky gate. It updates the active selection silently (for the compile-time
// resolver) and never notifies. ok=false means every backend is cooling and the
// pool is configured to fail.
func (c *Controller) Active(model string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return "", false
	}
	st.choose(time.Now())
	if st.active == "" {
		return "", false
	}
	return st.active, true
}

// Report cools a member after a retryable failure and re-selects.
func (c *Controller) Report(model, backend, class string, retryAfterSec int) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil || !st.has(backend) {
		c.mu.Unlock()
		return
	}
	dur := st.cooldownDur
	if retryAfterSec > 0 {
		dur = time.Duration(retryAfterSec) * time.Second
	}
	st.cooldown[backend] = time.Now().Add(dur)
	changed := st.choose(time.Now())
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
}

// ObserveStats records observations for a backend and re-selects. A ttftMs <= 0
// means "no ttft sample"; a cost < 0 means "no cost sample".
func (c *Controller) ObserveStats(model, backend string, ttftMs, cost float64) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil || !st.has(backend) {
		c.mu.Unlock()
		return
	}
	s := st.stats[backend]
	if s == nil {
		s = &backendStat{}
		st.stats[backend] = s
	}
	if ttftMs > 0 {
		if s.samples == 0 {
			s.ttftMs = ttftMs
		} else {
			s.ttftMs = statAlpha*ttftMs + (1-statAlpha)*s.ttftMs
		}
		s.samples++
	}
	if cost >= 0 {
		if s.cost == 0 {
			s.cost = cost
		} else {
			s.cost = statAlpha*cost + (1-statAlpha)*s.cost
		}
	}
	s.at = time.Now()
	changed := st.choose(time.Now())
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
}

// Rotate manually advances to the next backend in the current ranking
// (excluding the active one) and re-selects. Returns the active backend.
func (c *Controller) Rotate(model string) (string, bool) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil {
		c.mu.Unlock()
		return "", false
	}
	now := time.Now()
	ranked := st.rank(now)
	changed := false
	for _, backend := range ranked {
		if backend != st.active {
			changed = st.setActive(backend)
			break
		}
	}
	if !changed && st.onAllLimited == "force-least-recent" {
		if alt, ok := st.leastRecent(); ok {
			changed = st.setActive(alt)
		}
	}
	active := st.active
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
	return active, true
}

// ClearCooldowns drops all cooldowns for a pool without changing the active
// backend. Returns false when the pool is unknown.
func (c *Controller) ClearCooldowns(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return false
	}
	st.cooldown = map[string]time.Time{}
	return true
}

// RetryAfter reports seconds until the earliest cooled backend becomes healthy
// again (>=1), used when the whole pool is limited.
func (c *Controller) RetryAfter(model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return 0
	}
	now := time.Now()
	best := 0
	for _, backend := range st.backends {
		until, ok := st.cooldown[backend]
		if !ok || !now.Before(until) {
			return 1
		}
		seconds := int(time.Until(until).Seconds()) + 1
		if best == 0 || seconds < best {
			best = seconds
		}
	}
	if best == 0 {
		best = 1
	}
	return best
}

// BackendStat is a read-only view of one backend's observations.
type BackendStat struct {
	TTFTMs  float64 `json:"ttft_ms"`
	Cost    float64 `json:"cost,omitempty"`
	Samples int     `json:"samples"`
}

// Status is a read-only snapshot for the management API.
type Status struct {
	Model        string                 `json:"model"`
	Policy       string                 `json:"policy"`
	Backends     []string               `json:"backends"`
	Active       string                 `json:"active"`
	OnAllLimited string                 `json:"on_all_limited"`
	CooldownSec  int                    `json:"cooldown_sec"`
	ReselectTMs  int                    `json:"reselect_ttft_ms,omitempty"`
	Cooling      map[string]int         `json:"cooling,omitempty"` // backend → seconds remaining
	Stats        map[string]BackendStat `json:"stats,omitempty"`
}

func (c *Controller) Status(model string) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return Status{}, false
	}
	cooling := map[string]int{}
	stats := map[string]BackendStat{}
	now := time.Now()
	for _, backend := range st.backends {
		if until, ok := st.cooldown[backend]; ok && now.Before(until) {
			cooling[backend] = int(time.Until(until).Seconds()) + 1
		}
		if s := st.stats[backend]; s != nil && s.samples > 0 {
			stats[backend] = BackendStat{TTFTMs: s.ttftMs, Cost: s.cost, Samples: s.samples}
		}
	}
	return Status{
		Model:        model,
		Policy:       st.policy,
		Backends:     append([]string(nil), st.backends...),
		Active:       st.active,
		OnAllLimited: st.onAllLimited,
		CooldownSec:  int(st.cooldownDur / time.Second),
		ReselectTMs:  st.reselectTTFTMs,
		Cooling:      cooling,
		Stats:        stats,
	}, true
}

// ---- ranking ----

// choose applies the sort and sticky gate, updating active. Returns true when
// the active backend changed.
func (s *state) choose(now time.Time) bool {
	ranked := s.rank(now)
	if len(ranked) == 0 {
		if s.onAllLimited == "force-least-recent" {
			if alt, ok := s.leastRecent(); ok {
				return s.setActive(alt)
			}
		}
		// fail policy: clear the active so the caller can surface "all limited"
		return s.setActive("")
	}
	top := ranked[0]
	prev := s.active
	if prev == "" || !s.has(prev) || s.cooling(prev, now) {
		return s.setActive(top)
	}
	if s.reselectTTFTMs > 0 {
		if ttft, _ := s.metrics(prev); ttft > float64(s.reselectTTFTMs) && top != prev {
			return s.setActive(top)
		}
	}
	return false
}

func (s *state) setActive(backend string) bool {
	if s.active == backend {
		return false
	}
	s.active = backend
	return true
}

// rank returns the non-cooling backends ordered by the active policy. Backends
// without observations sort last on the ttft key (unknown = +Inf) but remain
// selectable when they are the only candidates.
func (s *state) rank(now time.Time) []string {
	out := make([]string, 0, len(s.backends))
	for _, backend := range s.backends {
		if !s.cooling(backend, now) {
			out = append(out, backend)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return s.less(out[i], out[j])
	})
	return out
}

func (s *state) less(a, b string) bool {
	aTTFT, aPrice := s.metrics(a)
	bTTFT, bPrice := s.metrics(b)
	if s.policy == PolicyPriceFirst {
		if aPrice != bPrice {
			return aPrice < bPrice
		}
		if aTTFT != bTTFT {
			return aTTFT < bTTFT
		}
	} else {
		if aTTFT != bTTFT {
			return aTTFT < bTTFT
		}
		if aPrice != bPrice {
			return aPrice < bPrice
		}
	}
	return s.index(a) < s.index(b)
}

// metrics returns the effective (ttft, price) used for ordering. ttft defaults
// to +Inf when unobserved; price prefers the observed cost over the configured
// starting price when an observation exists.
func (s *state) metrics(backend string) (ttft, price float64) {
	ttft = math.Inf(1)
	price = s.price[backend]
	if st := s.stats[backend]; st != nil && st.samples > 0 {
		ttft = st.ttftMs
		if st.cost > 0 {
			price = st.cost
		}
	}
	return ttft, price
}

func (s *state) index(backend string) int {
	for i, b := range s.backends {
		if b == backend {
			return i
		}
	}
	return len(s.backends)
}

func (s *state) has(backend string) bool {
	for _, b := range s.backends {
		if b == backend {
			return true
		}
	}
	return false
}

func (s *state) cooling(backend string, now time.Time) bool {
	until, ok := s.cooldown[backend]
	return ok && now.Before(until)
}

func (s *state) leastRecent() (string, bool) {
	var best string
	var bestUntil time.Time
	for _, backend := range s.backends {
		until := s.cooldown[backend]
		if best == "" || until.Before(bestUntil) {
			best, bestUntil = backend, until
		}
	}
	return best, best != ""
}
