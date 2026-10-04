// Package pool implements the control-plane side of backend pools: a pool maps
// one logical model (alias) to an ordered list of backend model ids. Selection is
// sticky — the active backend keeps serving until it reports a retryable upstream
// failure, at which point it is cooled and the next healthy backend is promoted.
// There is deliberately no round-robin: it would defeat provider-side prefix/KV
// cache reuse.
package pool

import (
	"sync"
	"time"
)

// Config registers one pool.
type Config struct {
	Model        string   // alias clients call
	Backend      []string // ordered backend model ids
	CooldownSec  int      // fallback cooldown when the upstream sends no retry-after
	OnAllLimited string   // "fail" (default) or "force-least-recent"
}

// Controller holds all pool state. It is safe for concurrent use.
type Controller struct {
	mu     sync.Mutex
	pools  map[string]*state
	notify func() // called (unlocked) when the active backend changes
}

type state struct {
	backends     []string
	active       string
	cooldown     map[string]time.Time
	cooldownDur  time.Duration
	onAllLimited string
}

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

// Register installs/replaces a pool. Empty/invalid configs are ignored.
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
	c.mu.Lock()
	defer c.mu.Unlock()
	// Preserve runtime health across hot reloads: only re-initialize a pool that
	// does not exist yet.
	if existing, ok := c.pools[cfg.Model]; ok {
		existing.backends = append([]string(nil), cfg.Backend...)
		existing.cooldownDur = cooldown
		existing.onAllLimited = onAll
		if !existing.has(existing.active) {
			existing.active = cfg.Backend[0]
		}
		for backend := range existing.cooldown {
			if !existing.has(backend) {
				delete(existing.cooldown, backend)
			}
		}
		return
	}
	c.pools[cfg.Model] = &state{
		backends:     append([]string(nil), cfg.Backend...),
		active:       cfg.Backend[0],
		cooldown:     map[string]time.Time{},
		cooldownDur:  cooldown,
		onAllLimited: onAll,
	}
}

// Active returns the backend to serve the next request. ok=false means every
// backend is cooling and the pool is configured to fail.
func (c *Controller) Active(model string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return "", false
	}
	now := time.Now()
	if !st.cooling(st.active, now) {
		return st.active, true
	}
	if next, ok := st.firstHealthy(now, ""); ok {
		st.active = next
		return next, true
	}
	if st.onAllLimited == "force-least-recent" {
		if next, ok := st.leastRecent(); ok {
			st.active = next
			return next, true
		}
	}
	return "", false
}

// Report cools a member after a retryable failure and, when it was the active
// one, promotes the next healthy backend.
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
	now := time.Now()
	st.cooldown[backend] = now.Add(dur)
	changed := false
	if backend == st.active {
		if next, ok := st.firstHealthy(now, backend); ok && next != st.active {
			st.active = next
			changed = true
		}
	}
	notify := c.notify
	c.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
}

// Rotate manually advances to the next healthy backend (excluding the current
// one) and notifies the control plane. It returns ok=false when the pool is
// unknown; when every backend is cooling the active choice is left unchanged and
// the caller can decide how to surface it.
func (c *Controller) Rotate(model string) (string, bool) {
	c.mu.Lock()
	st := c.pools[model]
	if st == nil {
		c.mu.Unlock()
		return "", false
	}
	now := time.Now()
	next, ok := st.firstHealthy(now, st.active)
	if !ok && st.onAllLimited == "force-least-recent" {
		if alt, found := st.leastRecent(); found && alt != st.active {
			next, ok = alt, true
		}
	}
	changed := false
	if ok && next != st.active {
		st.active = next
		changed = true
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

// Status is a read-only snapshot for the management API.
type Status struct {
	Model        string         `json:"model"`
	Backends     []string       `json:"backends"`
	Active       string         `json:"active"`
	OnAllLimited string         `json:"on_all_limited"`
	CooldownSec  int            `json:"cooldown_sec"`
	Cooling      map[string]int `json:"cooling,omitempty"` // backend → seconds remaining
}

func (c *Controller) Status(model string) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[model]
	if st == nil {
		return Status{}, false
	}
	cooling := map[string]int{}
	now := time.Now()
	for _, backend := range st.backends {
		if until, ok := st.cooldown[backend]; ok && now.Before(until) {
			cooling[backend] = int(time.Until(until).Seconds()) + 1
		}
	}
	return Status{
		Model:        model,
		Backends:     append([]string(nil), st.backends...),
		Active:       st.active,
		OnAllLimited: st.onAllLimited,
		CooldownSec:  int(st.cooldownDur / time.Second),
		Cooling:      cooling,
	}, true
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

func (s *state) firstHealthy(now time.Time, skip string) (string, bool) {
	for _, backend := range s.backends {
		if backend == skip {
			continue
		}
		if !s.cooling(backend, now) {
			return backend, true
		}
	}
	return "", false
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
