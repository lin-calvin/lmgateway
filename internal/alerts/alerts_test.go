package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

type fakeNotifier struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeNotifier) Notify(_ context.Context, a *Alert, event string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event+":"+a.Rule+":"+a.Subject)
	return nil
}

func waitEvents(t *testing.T, f *fakeNotifier, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.list(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return f.list()
}

func (f *fakeNotifier) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newEngine(t *testing.T, notifier Notifier) (*Engine, store.TSStore, *clock, store.JSONStore) {
	t.Helper()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	js := mem.NewJSON()
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	cfg := config.DefaultAlertsCfg()
	cfg.WindowSec = 300
	cfg.CooldownSec = 1800
	cfg.ResolveAfter = 2
	cfg.QuotaDenialMin = 3
	cfg.QuotaDenialCrit = 10
	cfg.UpstreamErrorMin = 4
	cfg.UpstreamErrorRate = 0.5
	e := New(ts, js, func() config.AlertsCfg { return cfg }, Options{Now: c.now, Notifier: notifier})
	return e, ts, c, js
}

func seedError(t *testing.T, ts store.TSStore, at time.Time, tags map[string]string) {
	t.Helper()
	if err := ts.Append(context.Background(), &store.TSRecord{
		Ts: at, Stream: store.StreamRequestError, Tags: tags,
		Fields: map[string]any{"status_code": float64(429)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaAlertFiresCooldownAndResolves(t *testing.T) {
	n := &fakeNotifier{}
	e, ts, c, _ := newEngine(t, n)
	ctx := context.Background()
	seed := func() {
		for i := 0; i < 5; i++ {
			seedError(t, ts, c.now(), map[string]string{
				"tenant": "acme", "key": "k1", "kind": "policy", "class": "rate_limit", "reason": "rpm_exceeded",
			})
		}
		if err := ts.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// 1) 触发
	seed()
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	active := e.Active()
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1 (%+v)", len(active), active)
	}
	a := active[0]
	if a.Rule != RuleQuotaExhausted || a.Subject != "key:k1" || a.Severity != SeverityWarning {
		t.Fatalf("unexpected alert: %+v", a)
	}
	if got := waitEvents(t, n, 1); len(got) != 1 || got[0] != "firing:quota_exhausted:key:k1" {
		t.Fatalf("notifications = %v", got)
	}

	// 2) 冷却期内持续触发：不重复推送
	// 推进到上一批记录滑出统计窗口，保证这次的值仍触发同一级别（不会因升级级别而重发）。
	c.advance(6 * time.Minute)
	seed()
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := n.list(); len(got) != 1 {
		t.Fatalf("cooldown must suppress repeat notifications, got %v", got)
	}

	// 3) 越过冷却：再次推送
	c.advance(31 * time.Minute)
	seed()
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := waitEvents(t, n, 2); len(got) != 2 {
		t.Fatalf("after cooldown expected another notification, got %v", got)
	}

	// 4) 信号滑出统计窗口：连续 ResolveAfter(2) 次评估后自动解除
	c.advance(6 * time.Minute)
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.Active()) != 1 {
		t.Fatal("alert resolved too early")
	}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.Active()) != 0 {
		t.Fatalf("alert should be resolved, got %+v", e.Active())
	}
	events := waitEvents(t, n, 3)
	if len(events) < 3 || events[len(events)-1] != "resolved:quota_exhausted:key:k1" {
		t.Fatalf("resolved notification missing: %v", events)
	}
}

// TestQuotaAlertAttributesToDeniedLayer 证明配额告警指向真正打满的那一层：
// 拒绝发生在租户层时，告警对象必须是 tenant:xxx，而不是笼统的 key。
func TestQuotaAlertAttributesToDeniedLayer(t *testing.T) {
	n := &fakeNotifier{}
	e, ts, c, _ := newEngine(t, n)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		seedError(t, ts, c.now(), map[string]string{
			"tenant": "acme", "project": "web", "key": "k1",
			"kind": "policy", "class": "rate_limit",
			"reason": "daily_cost_exceeded", "scope": "tenant:acme",
		})
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	active := e.Active()
	if len(active) != 1 {
		t.Fatalf("active = %+v", active)
	}
	if active[0].Subject != "tenant:acme" {
		t.Fatalf("alert must point at the denied layer, got %q (labels=%v)", active[0].Subject, active[0].Labels)
	}
	if active[0].TenantID != "acme" || active[0].KeyID != "k1" {
		t.Fatalf("alert lost scope metadata: %+v", active[0])
	}
}

func TestUpstreamMassFailureRequiresCountAndRate(t *testing.T) {
	n := &fakeNotifier{}
	e, ts, c, _ := newEngine(t, n)
	ctx := context.Background()

	// 4 次失败 + 1 次成功 → 80% 失败率，达到阈值。
	for i := 0; i < 4; i++ {
		seedError(t, ts, c.now(), map[string]string{"provider": "openai", "model": "gpt-x", "kind": "upstream", "class": "upstream"})
	}
	if err := ts.Append(ctx, &store.TSRecord{
		Ts: c.now(), Stream: store.StreamSpend,
		Tags: map[string]string{"provider": "openai", "model": "gpt-x"}, Fields: map[string]any{"cost": 0.1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	active := e.Active()
	if len(active) != 1 || active[0].Rule != RuleUpstreamMassFailure {
		t.Fatalf("active = %+v", active)
	}
	if active[0].Provider != "openai" || active[0].ErrorRate < 0.79 {
		t.Fatalf("unexpected alert: %+v", active[0])
	}

	// 低流量：只有 2 次失败（低于 min=4），不告警。
	e2, ts2, c2, _ := newEngine(t, &fakeNotifier{})
	for i := 0; i < 2; i++ {
		seedError(t, ts2, c2.now(), map[string]string{"provider": "openai", "kind": "upstream", "class": "upstream"})
	}
	if err := ts2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e2.Active()) != 0 {
		t.Fatalf("low-volume failures must not alert, got %+v", e2.Active())
	}
}

func TestEngineRestoresFiringStateWithoutRenotify(t *testing.T) {
	n := &fakeNotifier{}
	e, ts, c, js := newEngine(t, n)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		seedError(t, ts, c.now(), map[string]string{"tenant": "acme", "key": "k1", "kind": "policy", "class": "rate_limit", "reason": "rpm_exceeded"})
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.persist(ctx, e.Active()[0]); err != nil {
		t.Fatal(err)
	}

	// 新进程：从 JSONStore 恢复 firing 状态，不应把同一告警当新告警重复推送。
	n2 := &fakeNotifier{}
	e2 := New(ts, js, e.cfg, Options{Now: e.opt.Now, Notifier: n2})
	e2.load(ctx)
	if len(e2.Active()) != 1 {
		t.Fatalf("restored alerts = %d, want 1", len(e2.Active()))
	}
	if err := e2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := waitEvents(t, n2, 1); len(got) != 0 {
		t.Fatalf("restored alert must not re-notify, got %v", got)
	}
}

func TestWebhookPayload(t *testing.T) {
	type capture struct {
		payload WebhookPayload
		mu      sync.Mutex
	}
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		c.mu.Lock()
		c.payload = p
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &Webhook{URL: srv.URL, Client: srv.Client()}
	at := time.Now().UTC()
	a := &Alert{
		ID: "quota_exhausted/key-k1", Rule: RuleQuotaExhausted, Severity: SeverityCritical,
		Subject: "key:k1", KeyID: "k1", TenantID: "acme", Message: "boom", Value: 12, Threshold: 3,
		Count: 2, FirstSeenAt: at,
	}
	if err := w.Notify(context.Background(), a, "firing"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.payload.Event != "firing" || c.payload.Rule != RuleQuotaExhausted || c.payload.Value != 12 {
		t.Fatalf("payload = %+v", c.payload)
	}
}
