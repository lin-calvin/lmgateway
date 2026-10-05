// Package alerts 把两个"线上出事了只能靠翻日志"的场景变成可查、可推送的告警：
//
//   - quota_exhausted        配额打满（RPM/TPM/日预算/月预算被拒）
//   - upstream_mass_failure  上游大面积失败（按 provider 统计失败率）
//
// 数据来源是数据面写入的 "request_error" 时间序列（每次失败一条），成功数取自
// "spend"（有 usage 的成功请求）。评估是周期性的：比较窗口内计数/失败率与阈值，
// 触发后写入 JSONStore（重启不丢、不重复刷屏），并按冷却时间推送 webhook。
//
// 设计取舍：
//   - 状态机刻意简单：一个 (rule, subject) 最多一条 firing 告警；连续 N 次评估无
//     信号才自动解除。避免上游抖动导致告警风暴。
//   - 推送是异步 + 有超时 + 失败即丢：告警通道不能反过来拖垮网关。
//   - 不做时序数据库：只存当前状态 + 最近解除记录，评估窗口直接从 TS 读。
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
)

// 规则名（稳定标识，前端/文档按它分支）。
const (
	RuleQuotaExhausted      = "quota_exhausted"
	RuleUpstreamMassFailure = "upstream_mass_failure"
)

// 告警状态。
const (
	StateFiring   = "firing"
	StateResolved = "resolved"
)

// 严重级别。
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Prefix 告警状态在 JSONStore 中的键前缀。刻意与 setting/ 分开：
// setting/ 下的文档会被配置解析器当成配置项读取。
const Prefix = "alert/"

// ErrorStream 数据面写入的失败事件流。
const ErrorStream = "request_error"

// Alert 一条告警（当前状态 + 最近一次触发信息）。
type Alert struct {
	ID          string    `json:"id"`
	Rule        string    `json:"rule"`
	Severity    string    `json:"severity"`
	State       string    `json:"state"`
	Subject     string    `json:"subject"`
	TenantID    string    `json:"tenant_id,omitempty"`
	ProjectID   string    `json:"project_id,omitempty"`
	KeyID       string    `json:"key_id,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Message     string    `json:"message"`
	Value       float64   `json:"value"`
	Threshold   float64   `json:"threshold"`
	ErrorRate   float64   `json:"error_rate,omitempty"`
	Count       int       `json:"count"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	// LastNotifiedAt 上次推送时间。冷却基于它而不是 LastSeenAt——后者每轮都会刷新，
	// 否则持续触发的告警永远不会再次通知。
	LastNotifiedAt time.Time         `json:"last_notified_at,omitempty"`
	ResolvedAt     *time.Time        `json:"resolved_at,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// Query 查询条件。
type Query struct {
	State    string // firing|resolved|""(全部)
	Rule     string
	TenantID string
	Limit    int
	Offset   int
}

// Notifier 告警外发通道。返回错误只用于日志，不影响评估。
type Notifier interface {
	Notify(ctx context.Context, a *Alert, event string) error
}

// Options 引擎参数。
type Options struct {
	// Now 可注入时钟（测试用）。
	Now func() time.Time
	// Notifier 可选外发通道。
	Notifier Notifier
	// HTTPClient webhook 用；为 nil 时用默认带超时的 client。
	HTTPClient *http.Client
}

// Engine 告警引擎。
type Engine struct {
	ts  store.TSStore
	js  store.JSONStore
	cfg func() config.AlertsCfg
	opt Options

	mu        sync.Mutex
	active    map[string]*Alert // key = alertKey(rule, subject)
	clear     map[string]int    // key = alertKey, value = 连续无信号次数
	lastPrune time.Time
}

// New 构造引擎。cfg 为 nil 时使用默认阈值。
func New(ts store.TSStore, js store.JSONStore, cfg func() config.AlertsCfg, opt Options) *Engine {
	if cfg == nil {
		cfg = func() config.AlertsCfg { return config.DefaultAlertsCfg() }
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Engine{
		ts:     ts,
		js:     js,
		cfg:    cfg,
		opt:    opt,
		active: map[string]*Alert{},
		clear:  map[string]int{},
	}
}

// Start 周期评估。interval <= 0 时取配置里的 eval_interval_sec。
func (e *Engine) Start(ctx context.Context, interval time.Duration) {
	go func() {
		if interval <= 0 {
			interval = time.Duration(config.NormalizeAlertsCfg(e.cfg()).EvalIntervalSec) * time.Second
		}
		if interval <= 0 {
			interval = time.Minute
		}
		// 先加载已持久化的状态，避免重启后重复推送同一告警。
		e.load(ctx)
		_ = e.Run(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = e.Run(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Run 执行一次评估。
func (e *Engine) Run(ctx context.Context) error {
	cfg := config.NormalizeAlertsCfg(e.cfg())
	if cfg.Disabled {
		return nil
	}
	if e.ts == nil {
		return nil
	}
	now := e.opt.Now().UTC()
	window := time.Duration(cfg.WindowSec) * time.Second
	from := now.Add(-window)

	// 失败事件（含配额拒绝与上游失败）
	errs, err := e.ts.Query(ctx, store.TSQuery{Stream: ErrorStream, From: from, To: now})
	if err != nil {
		return fmt.Errorf("query error stream: %w", err)
	}
	// 成功请求数（失败率分母）——5 分钟窗口，一定在 raw retention 内，无需 daily。
	spend, err := e.ts.Query(ctx, store.TSQuery{Stream: "spend", From: from, To: now})
	if err != nil {
		return fmt.Errorf("query spend stream: %w", err)
	}

	current := map[string]*Alert{}
	for _, a := range evaluateQuota(errs, cfg, now, window) {
		current[alertKey(a.Rule, a.Subject)] = a
	}
	for _, a := range evaluateUpstream(errs, spend, cfg, now, window) {
		current[alertKey(a.Rule, a.Subject)] = a
	}
	e.reconcile(ctx, current, cfg, now)
	return nil
}

// reconcile 把本轮观察到的告警与内存状态合并：新触发/持续触发 → firing；
// 连续 N 轮无信号 → resolved。
func (e *Engine) reconcile(ctx context.Context, current map[string]*Alert, cfg config.AlertsCfg, now time.Time) {
	e.mu.Lock()
	var fired, resolved []*Alert
	for key, observed := range current {
		prev, existed := e.active[key]
		delete(e.clear, key)
		if existed && prev.State == StateFiring {
			observed.FirstSeenAt = prev.FirstSeenAt
			observed.Count = prev.Count
			observed.ID = prev.ID
		}
		observed.Count++
		observed.State = StateFiring
		observed.LastSeenAt = now
		if observed.FirstSeenAt.IsZero() {
			observed.FirstSeenAt = now
		}
		observed.ID = alertID(observed.Rule, observed.Subject)
		e.active[key] = observed
		// 新告警、级别升高、或已过冷却 → 推送
		shouldNotify := !existed || prev.State != StateFiring ||
			severityRank(observed.Severity) > severityRank(prev.Severity) ||
			now.Sub(prev.LastNotifiedAt) >= time.Duration(cfg.CooldownSec)*time.Second
		if shouldNotify {
			observed.LastNotifiedAt = now
			fired = append(fired, observed)
		} else {
			observed.LastNotifiedAt = prev.LastNotifiedAt
		}
	}
	for key, prev := range e.active {
		if _, still := current[key]; still {
			continue
		}
		if prev.State != StateFiring {
			continue
		}
		e.clear[key]++
		if e.clear[key] < cfg.ResolveAfter {
			continue
		}
		delete(e.clear, key)
		resolvedAt := now
		prev.State = StateResolved
		prev.ResolvedAt = &resolvedAt
		resolved = append(resolved, prev)
	}
	e.mu.Unlock()

	for _, a := range fired {
		if err := e.persist(ctx, a); err != nil {
			log.Printf("[alerts] persist failed id=%s: %v", a.ID, err)
		}
		log.Printf("[alerts] FIRING rule=%s severity=%s subject=%s value=%.0f threshold=%.0f message=%s",
			a.Rule, a.Severity, a.Subject, a.Value, a.Threshold, a.Message)
		e.notify(ctx, a, "firing")
	}
	for _, a := range resolved {
		if err := e.persist(ctx, a); err != nil {
			log.Printf("[alerts] persist failed id=%s: %v", a.ID, err)
		}
		log.Printf("[alerts] RESOLVED rule=%s subject=%s", a.Rule, a.Subject)
		e.notify(ctx, a, "resolved")
	}
	e.prune(ctx, cfg, now)
}

func (e *Engine) notify(ctx context.Context, a *Alert, event string) {
	cfg := config.NormalizeAlertsCfg(e.cfg())
	notifier := e.opt.Notifier
	if notifier == nil {
		// 配置可以热更新（改 webhook_url 不用重启），所以每次外发时再取一次。
		if strings.TrimSpace(cfg.WebhookURL) == "" {
			return
		}
		notifier = &Webhook{URL: cfg.WebhookURL, Client: e.opt.HTTPClient}
	}
	timeout := time.Duration(cfg.WebhookTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// 异步 + 有超时：外发失败不能影响评估循环。
	go func() {
		nctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := notifier.Notify(nctx, a, event); err != nil {
			log.Printf("[alerts] webhook delivery failed id=%s event=%s: %v", a.ID, event, err)
		}
	}()
}

// Active 返回内存中处于 firing 的告警（已解除的只在持久化里保留）。
func (e *Engine) Active() []*Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Alert, 0, len(e.active))
	for _, a := range e.active {
		if a.State != StateFiring {
			continue
		}
		clone := *a
		out = append(out, &clone)
	}
	sortAlerts(out)
	return out
}

// List 从持久化存储里查告警（含历史解除记录），分页返回。
func (e *Engine) List(ctx context.Context, q Query) ([]*Alert, int, error) {
	out := []*Alert{}
	if e.js == nil {
		// 没有持久化时退回内存视图，至少让 API 可用。
		for _, a := range e.Active() {
			if !matchAlert(a, q) {
				continue
			}
			out = append(out, a)
		}
		return page(out, q.Limit, q.Offset), len(out), nil
	}
	docs, err := e.js.List(ctx, Prefix)
	if err != nil {
		return nil, 0, err
	}
	byID := map[string]*Alert{}
	for _, d := range docs {
		var a Alert
		if err := json.Unmarshal(d.Data, &a); err != nil {
			continue
		}
		byID[a.ID] = &a
	}
	// 持久化只在状态变化时写；用内存里的实时值覆盖，API 才能看到最新的次数与时间。
	for _, live := range e.Active() {
		byID[live.ID] = live
	}
	for _, a := range byID {
		if !matchAlert(a, q) {
			continue
		}
		out = append(out, a)
	}
	sortAlerts(out)
	total := len(out)
	return page(out, q.Limit, q.Offset), total, nil
}

func (e *Engine) load(ctx context.Context) {
	if e.js == nil {
		return
	}
	docs, err := e.js.List(ctx, Prefix)
	if err != nil {
		log.Printf("[alerts] load state failed: %v", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range docs {
		var a Alert
		if err := json.Unmarshal(d.Data, &a); err != nil {
			continue
		}
		if a.State != StateFiring {
			continue
		}
		key := alertKey(a.Rule, a.Subject)
		e.active[key] = &a
	}
	if len(e.active) > 0 {
		log.Printf("[alerts] restored %d firing alerts", len(e.active))
	}
}

func (e *Engine) persist(ctx context.Context, a *Alert) error {
	if e.js == nil {
		return nil
	}
	_, err := e.js.Put(ctx, Prefix+alertID(a.Rule, a.Subject), a)
	return err
}

// prune 删除保留期之外的已解除告警。
func (e *Engine) prune(ctx context.Context, cfg config.AlertsCfg, now time.Time) {
	if e.js == nil || cfg.RetainDays <= 0 {
		return
	}
	e.mu.Lock()
	due := now.Sub(e.lastPrune) > 6*time.Hour
	if due {
		e.lastPrune = now
	}
	e.mu.Unlock()
	if !due {
		return
	}
	docs, err := e.js.List(ctx, Prefix)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -cfg.RetainDays)
	for _, d := range docs {
		var a Alert
		if err := json.Unmarshal(d.Data, &a); err != nil {
			continue
		}
		if a.State == StateFiring || a.ResolvedAt == nil || a.ResolvedAt.After(cutoff) {
			continue
		}
		if err := e.js.Delete(ctx, d.Key); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("[alerts] prune failed key=%s: %v", d.Key, err)
		}
	}
}

// ---------- 规则 ----------

// evaluateQuota 按 (key/tenant, reason) 统计窗口内的配额拒绝次数。
func evaluateQuota(errs []*store.TSRecord, cfg config.AlertsCfg, now time.Time, window time.Duration) []*Alert {
	type bucket struct {
		alert  *Alert
		count  float64
		reason string
	}
	buckets := map[string]*bucket{}
	for _, r := range errs {
		if r.Tags["kind"] != "policy" && r.Tags["class"] != "rate_limit" {
			continue
		}
		reason := r.Tags["reason"]
		if !isQuotaReason(reason) {
			continue
		}
		// 优先用"网关实际拒绝的那一层"（policy 拒绝会带 scope tag）；
		// 缺失时才退化为最细可用作用域 key → project → tenant。
		scopeKind, scopeID := deniedScopeOf(r)
		if scopeID == "" {
			continue
		}
		key := scopeKind + ":" + scopeID + "|" + reason
		b, ok := buckets[key]
		if !ok {
			b = &bucket{alert: &Alert{
				Rule:      RuleQuotaExhausted,
				Subject:   scopeKind + ":" + scopeID,
				TenantID:  r.Tags["tenant"],
				ProjectID: r.Tags["project"],
				KeyID:     r.Tags["key"],
				Reason:    reason,
				Labels: map[string]string{
					"scope":      scopeKind + ":" + scopeID,
					"reason":     reason,
					"window":     window.String(),
					"denied_via": r.Tags["scope"],
				},
			}, reason: reason}
			buckets[key] = b
		}
		b.count++
	}
	out := []*Alert{}
	for _, b := range buckets {
		if b.count < float64(cfg.QuotaDenialMin) {
			continue
		}
		severity := SeverityWarning
		if b.count >= float64(cfg.QuotaDenialCrit) {
			severity = SeverityCritical
		}
		a := b.alert
		a.Severity = severity
		a.Value = b.count
		a.Threshold = float64(cfg.QuotaDenialMin)
		a.Message = fmt.Sprintf("quota exhausted: %s hit %s %d times in %s", a.Subject, b.reason, int(b.count), window)
		out = append(out, a)
	}
	return out
}

// evaluateUpstream 按 provider 统计窗口内的上游失败次数与失败率。
func evaluateUpstream(errs, spend []*store.TSRecord, cfg config.AlertsCfg, now time.Time, window time.Duration) []*Alert {
	type bucket struct {
		count   float64
		success float64
		models  map[string]bool
	}
	buckets := map[string]*bucket{}
	for _, r := range errs {
		if r.Tags["kind"] != "upstream" {
			continue
		}
		provider := r.Tags["provider"]
		if provider == "" {
			provider = "(unknown)"
		}
		b, ok := buckets[provider]
		if !ok {
			b = &bucket{models: map[string]bool{}}
			buckets[provider] = b
		}
		b.count++
		if model := r.Tags["model"]; model != "" {
			b.models[model] = true
		}
	}
	for _, r := range spend {
		provider := r.Tags["provider"]
		if provider == "" {
			provider = "(unknown)"
		}
		b, ok := buckets[provider]
		if !ok {
			b = &bucket{models: map[string]bool{}}
			buckets[provider] = b
		}
		b.success++
	}
	out := []*Alert{}
	for provider, b := range buckets {
		if b.count < float64(cfg.UpstreamErrorMin) {
			continue
		}
		total := b.count + b.success
		rate := 0.0
		if total > 0 {
			rate = b.count / total
		}
		if rate < cfg.UpstreamErrorRate {
			continue
		}
		models := make([]string, 0, len(b.models))
		for m := range b.models {
			models = append(models, m)
		}
		sort.Strings(models)
		severity := SeverityWarning
		if rate >= 0.9 || b.count >= float64(cfg.UpstreamErrorMin)*5 {
			severity = SeverityCritical
		}
		out = append(out, &Alert{
			Rule:      RuleUpstreamMassFailure,
			Severity:  severity,
			Subject:   "provider:" + provider,
			Provider:  provider,
			Value:     b.count,
			Threshold: float64(cfg.UpstreamErrorMin),
			ErrorRate: rate,
			Message: fmt.Sprintf("upstream mass failure: provider %s had %d failures with %.1f%% error rate in %s (models: %s)",
				provider, int(b.count), rate*100, window, strings.Join(models, ",")),
			Labels: map[string]string{
				"provider": provider,
				"models":   strings.Join(models, ","),
				"window":   window.String(),
			},
		})
	}
	return out
}

func isQuotaReason(reason string) bool {
	switch reason {
	case "rpm_exceeded", "tpm_exceeded", "daily_cost_exceeded", "monthly_cost_exceeded":
		return true
	}
	return false
}

// deniedScopeOf 解析被拒绝的配额层级。scope tag 形如 "tenant:acme"。
func deniedScopeOf(r *store.TSRecord) (string, string) {
	if raw := r.Tags["scope"]; raw != "" {
		if kind, id, ok := strings.Cut(raw, ":"); ok && kind != "" && id != "" {
			return kind, id
		}
	}
	return scopeOf(r)
}

func scopeOf(r *store.TSRecord) (string, string) {
	if v := r.Tags["key"]; v != "" {
		return "key", v
	}
	if v := r.Tags["project"]; v != "" {
		return "project", v
	}
	if v := r.Tags["tenant"]; v != "" {
		return "tenant", v
	}
	return "", ""
}

// ---------- 工具 ----------

func alertKey(rule, subject string) string { return rule + "|" + subject }

// alertID 是持久化键的稳定标识：同一 (rule, subject) 永远映射到同一条记录，
// 因此重启后不会被当成新告警重复推送。subject 里的字符会被收敛到 [A-Za-z0-9:.-]，
// 避免 JSONStore 前缀查询（LIKE）遇到 % 和 _ 时过度匹配。
func alertID(rule, subject string) string {
	var b strings.Builder
	b.WriteString(rule)
	b.WriteByte('/')
	for _, r := range subject {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ':', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func severityRank(s string) int {
	if s == SeverityCritical {
		return 2
	}
	if s == SeverityWarning {
		return 1
	}
	return 0
}

func matchAlert(a *Alert, q Query) bool {
	if q.State != "" && a.State != q.State {
		return false
	}
	if q.Rule != "" && a.Rule != q.Rule {
		return false
	}
	if q.TenantID != "" && a.TenantID != q.TenantID {
		return false
	}
	return true
}

func sortAlerts(items []*Alert) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].State != items[j].State {
			return items[i].State == StateFiring // firing 在前
		}
		if !items[i].LastSeenAt.Equal(items[j].LastSeenAt) {
			return items[i].LastSeenAt.After(items[j].LastSeenAt)
		}
		return items[i].ID < items[j].ID
	})
}

func page(items []*Alert, limit, offset int) []*Alert {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []*Alert{}
	}
	items = items[offset:]
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// ---------- Webhook ----------

// Webhook POST 告警 JSON 到指定 URL，payload 与 WebhookPayload 一致。
type Webhook struct {
	URL    string
	Client *http.Client
}

// WebhookPayload 外发报文。故意做成自解释的扁平结构：接收端不需要懂网关内部模型。
type WebhookPayload struct {
	Event      string    `json:"event"` // firing | resolved
	ID         string    `json:"id"`
	Rule       string    `json:"rule"`
	Severity   string    `json:"severity"`
	Subject    string    `json:"subject"`
	TenantID   string    `json:"tenant_id,omitempty"`
	ProjectID  string    `json:"project_id,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Message    string    `json:"message"`
	Value      float64   `json:"value"`
	Threshold  float64   `json:"threshold"`
	ErrorRate  float64   `json:"error_rate,omitempty"`
	Count      int       `json:"count"`
	FiredAt    time.Time `json:"fired_at"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// Notify 实现 Notifier。
func (w *Webhook) Notify(ctx context.Context, a *Alert, event string) error {
	if w == nil || w.URL == "" {
		return nil
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	payload := WebhookPayload{
		Event: event, ID: a.ID, Rule: a.Rule, Severity: a.Severity, Subject: a.Subject,
		TenantID: a.TenantID, ProjectID: a.ProjectID, KeyID: a.KeyID, Provider: a.Provider,
		Message: a.Message, Value: a.Value, Threshold: a.Threshold, ErrorRate: a.ErrorRate,
		Count: a.Count, FiredAt: a.FirstSeenAt,
	}
	if a.ResolvedAt != nil {
		payload.ResolvedAt = *a.ResolvedAt
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
