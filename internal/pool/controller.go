// Package pool implements the control-plane side of backend pools: a pool maps
// one logical model alias to an ordered list of backend model ids.
//
// Each backend has a 4-state health:
//
//	normal · slow · ratelimit · dead
//
// Selection ranks the eligible backends (normal, slow) by a policy; a cooled or
// dead backend is skipped. Recovery is driven by the periodic probe: a probe
// success revives a backend, a probe failure cools it. When every backend of a
// pool is ineligible the pool falls back to a configured target (another pool or
// a plain model id), which normal routing resolves.
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

// Health is the 4-state backend health.
type Health string

const (
	HealthNormal    Health = "normal"
	HealthSlow      Health = "slow"
	HealthRateLimit Health = "ratelimit"
	HealthDead      Health = "dead"
)

// Config registers one pool.
type Config struct {
	Model          string             // alias clients call
	Backend        []string           // ordered backend model ids
	CooldownSec    int                // fallback cooldown when the upstream sends no retry-after
	OnAllLimited   string             // "fail" (default) or "force-least-recent"
	Policy         string             // speed_first (default) or price_first
	ReselectTTFTMs int                // absolute trigger: switch when active ttft exceeds this
	Price          map[string]float64 // backend model id → price (0 for subscription plans)
	Fallback       string             // last-resort routing target (pool name or model id); never probed
}

// Reason is the typed cause of a transition.
type Reason string

const (
	ReasonFault    Reason = "fault"    // a backend health fault moved the active target
	ReasonRecover  Reason = "recover"  // a backend recovered
	ReasonRotate   Reason = "rotate"   // manual rotate
	ReasonReselect Reason = "reselect" // stats-driven reselect
)

// Event describes a pool transition, emitted only when the active routing target
// changes (so From != To always). Subscribers render it via Name/Payload.
type Event struct {
	Model      string            `json:"pool"`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	Reason     Reason            `json:"reason,omitempty"`
	Class      string            `json:"class,omitempty"` // fault class, when Reason==ReasonFault
	Downgraded bool              `json:"downgraded,omitempty"`
	At         time.Time         `json:"at"`
	Health     map[string]string `json:"health,omitempty"`
}

// Name is the webhook event type.
func (ev Event) Name() string {
	switch {
	case ev.Downgraded:
		return "pool.downgrade"
	case ev.Reason == ReasonFault:
		return "pool.fault"
	case ev.Reason == ReasonRecover:
		return "pool.recover"
	case ev.Reason == ReasonRotate:
		return "pool.rotate"
	case ev.Reason == ReasonReselect:
		return "pool.reselect"
	default:
		return "pool.change"
	}
}

// Payload is the JSON body posted to the events webhook.
func (ev Event) Payload() map[string]any {
	reason := string(ev.Reason)
	if ev.Reason == ReasonFault && ev.Class != "" {
		reason = "fault:" + ev.Class
	}
	return map[string]any{
		"event":      ev.Name(),
		"pool":       ev.Model,
		"from":       ev.From,
		"to":         ev.To,
		"reason":     reason,
		"downgraded": ev.Downgraded,
		"at":         ev.At.UTC().Format(time.RFC3339),
		"health":     ev.Health,
	}
}

// Controller holds all pool state. It is safe for concurrent use.
type Controller struct {
	mu     sync.Mutex
	pools  map[string]*state
	notify func(Event) // called (unlocked) when a pool changes
}

type state struct {
	backends       []string
	active         string
	fallback       string
	policy         string
	reselectTTFTMs int
	price          map[string]float64
	cooldownDur    time.Duration
	onAllLimited   string
	stats          map[string]*backendStat
	faults         map[string]*fault
}

type backendStat struct {
	ttftMs  float64
	cost    float64
	samples int
	at      time.Time
}

type fault struct {
	kind   Health // HealthRateLimit or HealthDead
	until  time.Time
	reason string
	since  time.Time
}

const statAlpha = 0.35

func NewController() *Controller {
	return &Controller{pools: map[string]*state{}}
}

// SetNotifier registers a callback invoked whenever a pool changes.
func (c *Controller) SetNotifier(fn func(Event)) {
	c.mu.Lock()
	c.notify = fn
	c.mu.Unlock()
}

// eventLocked builds the transition event for a method that may have just
// changed st.active. ok=false means the active backend did not change.
// Caller holds c.mu.
func (c *Controller) eventLocked(model string, st *state, before string, reason Reason, class string, now time.Time) (Event, bool) {
	if st.active == before {
		return Event{}, false
	}
	health := make(map[string]string, len(st.backends))
	for _, backend := range st.backends {
		health[backend] = string(st.healthOf(backend, now))
	}
	return Event{
		Model:      model,
		From:       before,
		To:         st.active,
		Reason:     reason,
		Class:      class,
		Downgraded: st.fallback != "" && st.active == st.fallback,
		At:         now,
		Health:     health,
	}, true
}

// Register installs/replaces a pool, preserving runtime health/stats across hot
// reloads.
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
		existing.fallback = cfg.Fallback
		existing.cooldownDur = cooldown
		existing.policy = policy
		existing.reselectTTFTMs = cfg.ReselectTTFTMs
		existing.price = price
		existing.onAllLimited = onAll
		if !existing.has(existing.active) {
			existing.active = cfg.Backend[0]
		}
		for b := range existing.stats {
			if !existing.has(b) {
				delete(existing.stats, b)
			}
		}
		for b := range existing.faults {
			if !existing.has(b) {
				delete(existing.faults, b)
			}
		}
		return
	}
	c.pools[cfg.Model] = &state{
		backends:       append([]string(nil), cfg.Backend...),
		active:         cfg.Backend[0],
		fallback:       cfg.Fallback,
		policy:         policy,
		reselectTTFTMs: cfg.ReselectTTFTMs,
		price:          price,
		cooldownDur:    cooldown,
		onAllLimited:   onAll,
		stats:          map[string]*backendStat{},
		faults:         map[string]*fault{},
	}
}

// Active returns the routing target to serve the next request (a backend id, or
// the fallback name when every backend is ineligible). It updates the selection
// silently (compile-time resolver) and never notifies. ok=false means there is
// no target at all (no fallback and on_all_limited=fail).
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

// Report applies a failure to a backend of a known pool (used by the probe).
func (c *Controller) Report(model, backend, class string, retryAfterSec int) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil || !st.has(backend) {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	before := st.active
	st.applyFault(backend, class, retryAfterSec, now)
	ev, changed := c.eventLocked(model, st, before, ReasonFault, class, now)
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify(ev)
	}
}

// ReportBackend applies a failure to every pool that owns the backend (used by
// the live reporter, which only knows the serving backend id).
func (c *Controller) ReportBackend(backend, class string, retryAfterSec int) {
	if backend == "" {
		return
	}
	c.mu.Lock()
	now := time.Now()
	var events []Event
	for model, st := range c.pools {
		if !st.has(backend) {
			continue
		}
		before := st.active
		st.applyFault(backend, class, retryAfterSec, now)
		if ev, ok := c.eventLocked(model, st, before, ReasonFault, class, now); ok {
			events = append(events, ev)
		}
	}
	notify := c.notify
	c.mu.Unlock()
	if notify != nil {
		for _, ev := range events {
			notify(ev)
		}
	}
}

// Recover clears any fault for a backend (probe success, or a manual reset).
// Returns false when the pool or backend is unknown.
func (c *Controller) Recover(model, backend string) bool {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil || !st.has(backend) {
		c.mu.Unlock()
		return false
	}
	now := time.Now()
	before := st.active
	delete(st.faults, backend)
	st.choose(now)
	ev, activeChanged := c.eventLocked(model, st, before, ReasonRecover, "", now)
	notify := c.notify
	c.mu.Unlock()
	if activeChanged && notify != nil {
		notify(ev)
	}
	return true
}

// RecoverBackend clears any fault for a backend across every pool that owns it.
// Used on a *successful* request: if a backend served a request, it is healthy
// (this revives `dead` too — e.g. an account topped back up).
func (c *Controller) RecoverBackend(backend string) {
	if backend == "" {
		return
	}
	c.mu.Lock()
	now := time.Now()
	var events []Event
	for model, st := range c.pools {
		if !st.has(backend) {
			continue
		}
		before := st.active
		delete(st.faults, backend)
		st.choose(now)
		if ev, ok := c.eventLocked(model, st, before, ReasonRecover, "", now); ok {
			events = append(events, ev)
		}
	}
	notify := c.notify
	c.mu.Unlock()
	if notify != nil {
		for _, ev := range events {
			notify(ev)
		}
	}
}

// ObserveStats records ttft/cost observations for a backend and re-selects.
// ttftMs <= 0 = no ttft sample; cost < 0 = no cost sample.
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
	now := time.Now()
	s.at = now
	before := st.active
	changed := st.choose(now)
	ev, _ := c.eventLocked(model, st, before, ReasonReselect, "", now)
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify(ev)
	}
}

// Rotate manually advances to the next eligible backend (or the fallback).
func (c *Controller) Rotate(model string) (string, bool) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil {
		c.mu.Unlock()
		return "", false
	}
	now := time.Now()
	before := st.active
	changed := false
	for _, b := range st.eligible(now) {
		if b != st.active {
			changed = st.setActive(b)
			break
		}
	}
	if !changed && st.fallback != "" {
		changed = st.setActive(st.fallback)
	}
	active := st.active
	ev, _ := c.eventLocked(model, st, before, ReasonRotate, "", now)
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify(ev)
	}
	return active, true
}

// ClearCooldowns drops all faults for a pool.
func (c *Controller) ClearCooldowns(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return false
	}
	st.faults = map[string]*fault{}
	return true
}

// RetryAfter reports seconds until the earliest cooled backend recovers.
func (c *Controller) RetryAfter(model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return 0
	}
	now := time.Now()
	best := 0
	for _, f := range st.faults {
		if f.kind != HealthRateLimit || !now.Before(f.until) {
			continue
		}
		seconds := int(time.Until(f.until).Seconds()) + 1
		if best == 0 || seconds < best {
			best = seconds
		}
	}
	if best == 0 {
		best = 1
	}
	return best
}

// BackendStatus is a read-only view of one backend.
type BackendStatus struct {
	Health   string  `json:"health"`
	TTFTMs   float64 `json:"ttft_ms,omitempty"`
	Cost     float64 `json:"cost,omitempty"`
	Samples  int     `json:"samples,omitempty"`
	UntilSec int     `json:"until_sec,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// Status is a read-only snapshot for the management API.
type Status struct {
	Model        string                   `json:"model"`
	Policy       string                   `json:"policy"`
	Active       string                   `json:"active"`
	Fallback     string                   `json:"fallback,omitempty"`
	Downgraded   bool                     `json:"downgraded,omitempty"`
	Backends     []string                 `json:"backends"`
	OnAllLimited string                   `json:"on_all_limited"`
	CooldownSec  int                      `json:"cooldown_sec"`
	ReselectTMs  int                      `json:"reselect_ttft_ms,omitempty"`
	Health       map[string]BackendStatus `json:"health,omitempty"`
}

func (c *Controller) Status(model string) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return Status{}, false
	}
	now := time.Now()
	health := map[string]BackendStatus{}
	for _, backend := range st.backends {
		health[backend] = st.backendStatus(backend, now)
	}
	return Status{
		Model:        model,
		Policy:       st.policy,
		Active:       st.active,
		Fallback:     st.fallback,
		Downgraded:   st.fallback != "" && st.active == st.fallback,
		Backends:     append([]string(nil), st.backends...),
		OnAllLimited: st.onAllLimited,
		CooldownSec:  int(st.cooldownDur / time.Second),
		ReselectTMs:  st.reselectTTFTMs,
		Health:       health,
	}, true
}

// ---- internal ----

func (s *state) applyFault(backend, class string, retryAfterSec int, now time.Time) bool {
	f := newFault(class, retryAfterSec, now, s.cooldownDur)
	if f == nil {
		return false
	}
	s.faults[backend] = f
	return s.choose(now)
}

func newFault(class string, retryAfterSec int, now time.Time, cooldown time.Duration) *fault {
	switch class {
	case "rate_limit", "overloaded":
		dur := cooldown
		if retryAfterSec > 0 {
			dur = time.Duration(retryAfterSec) * time.Second
		}
		return &fault{kind: HealthRateLimit, until: now.Add(dur), reason: class, since: now}
	case "quota", "network", "auth", "upstream":
		return &fault{kind: HealthDead, reason: class, since: now}
	default:
		// invalid / unknown: not a backend-health signal
		return nil
	}
}

func (s *state) healthOf(backend string, now time.Time) Health {
	if f := s.faults[backend]; f != nil {
		if f.kind == HealthDead {
			return HealthDead
		}
		if now.Before(f.until) {
			return HealthRateLimit
		}
		delete(s.faults, backend)
	}
	if s.reselectTTFTMs > 0 {
		if ttft, ok := s.ttft(backend); ok && ttft > float64(s.reselectTTFTMs) {
			return HealthSlow
		}
	}
	return HealthNormal
}

func (s *state) eligible(now time.Time) []string {
	out := make([]string, 0, len(s.backends))
	for _, backend := range s.backends {
		switch s.healthOf(backend, now) {
		case HealthNormal, HealthSlow:
			out = append(out, backend)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return s.less(out[i], out[j]) })
	return out
}

// choose applies the ranking + sticky gate + fallback, updating active.
func (s *state) choose(now time.Time) bool {
	eligible := s.eligible(now)
	if len(eligible) == 0 {
		target := ""
		switch {
		case s.fallback != "":
			target = s.fallback
		case s.onAllLimited == "force-least-recent":
			target = s.leastRecentFault(now)
		}
		return s.setActive(target)
	}
	top := eligible[0]
	if s.has(s.active) {
		if h := s.healthOf(s.active, now); h == HealthNormal {
			return false // sticky
		}
		// active is slow / ratelimit / dead → move to the best
	}
	return s.setActive(top)
}

func (s *state) setActive(target string) bool {
	if s.active == target {
		return false
	}
	s.active = target
	return true
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

func (s *state) metrics(backend string) (ttft, price float64) {
	ttft = math.Inf(1)
	if v, ok := s.ttft(backend); ok {
		ttft = v
	}
	price = s.price[backend]
	if st := s.stats[backend]; st != nil && st.cost > 0 {
		price = st.cost
	}
	return ttft, price
}

func (s *state) ttft(backend string) (float64, bool) {
	if st := s.stats[backend]; st != nil && st.samples > 0 {
		return st.ttftMs, true
	}
	return 0, false
}

func (s *state) backendStatus(backend string, now time.Time) BackendStatus {
	bs := BackendStatus{Health: string(s.healthOf(backend, now))}
	if st := s.stats[backend]; st != nil && st.samples > 0 {
		bs.TTFTMs = st.ttftMs
		bs.Cost = st.cost
		bs.Samples = st.samples
	}
	if f := s.faults[backend]; f != nil {
		bs.Reason = f.reason
		if f.kind == HealthRateLimit && now.Before(f.until) {
			bs.UntilSec = int(time.Until(f.until).Seconds()) + 1
		}
	}
	return bs
}

func (s *state) leastRecentFault(now time.Time) string {
	var best string
	var bestUntil time.Time
	for _, backend := range s.backends {
		f := s.faults[backend]
		if f == nil {
			continue
		}
		if f.kind == HealthRateLimit && now.Before(f.until) {
			if best == "" || f.until.Before(bestUntil) {
				best, bestUntil = backend, f.until
			}
			continue
		}
		if best == "" {
			best = backend
		}
	}
	return best
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
