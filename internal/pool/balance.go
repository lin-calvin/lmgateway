package pool

import (
	"context"
	"log"
	"time"

	"lmgateway/internal/balance"
	"lmgateway/internal/packet"
)

// BalanceTarget is the balance reader resolved for a backend's provider.
type BalanceTarget struct {
	Config   balance.Config
	APIKey   string
	Interval time.Duration
}

// BalanceResolver maps a backend model id to its provider's balance reader;
// ok=false means the backend's provider has no balance reader configured.
type BalanceResolver func(backend string) (BalanceTarget, bool)

// RecoverBalances revives backends that are dead due to a quota/balance error
// once their provider reports the account is funded again (instead of spending
// probes against a known-broke account). It only inspects quota-dead backends,
// and a fetch error never revives. `last` (keyed pool|backend) gates polling.
func (c *Controller) RecoverBalances(ctx context.Context, resolve BalanceResolver, last map[string]time.Time) {
	type target struct{ model, backend string }
	var targets []target
	c.mu.Lock()
	for model, st := range c.pools {
		for _, backend := range st.backends {
			f := st.faults[backend]
			if f != nil && f.kind == HealthDead && f.reason == packet.ClassQuota {
				targets = append(targets, target{model: model, backend: backend})
			}
		}
	}
	c.mu.Unlock()

	now := time.Now()
	for _, t := range targets {
		bt, ok := resolve(t.backend)
		if !ok {
			continue
		}
		key := t.model + "|" + t.backend
		if since := last[key]; !since.IsZero() && now.Sub(since) < bt.Interval {
			continue
		}
		funded, _, err := balance.Fetch(ctx, bt.Config, bt.APIKey)
		if err != nil {
			// transient error: do not consume the interval, retry next tick
			log.Printf("[balance] pool=%s backend=%s fetch failed: %v", t.model, t.backend, err)
			continue
		}
		last[key] = now
		if funded {
			log.Printf("[balance] pool=%s backend=%s recovered, reviving", t.model, t.backend)
			c.Recover(t.model, t.backend)
		}
	}
}
