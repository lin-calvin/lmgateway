package tenancy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 拒绝原因码(会出现在给客户端的错误里,前端也用它做文案;告警按它归类)。
const (
	ReasonRPM         = "rpm_exceeded"
	ReasonTPM         = "tpm_exceeded"
	ReasonDailyCost   = "daily_cost_exceeded"
	ReasonMonthlyCost = "monthly_cost_exceeded"
)

// QuotaScope 一层配额：对象、展示名与该层的限额。
//
// 三层（key → project → tenant）是三个**互相独立**的预算：任何一层超限都拒绝，
// 每层都按本次请求的全额计费（平摊会让上层预算被低估）。
type QuotaScope struct {
	Scope string // 记账键："key:k1" / "project:web" / "tenant:acme"
	Label string // 人类可读，进拒绝消息与告警（如 "project web"）

	RPMLimit         int
	TPMLimit         int
	DailyCostLimit   float64
	MonthlyCostLimit float64
}

// ScopesFor 组装一次数据面请求要判定的配额层级。
//
// 限额来源：API key 自身 + 所属 project + 所属 tenant。
// 任一层为空表示"不限"，但仍会参与记账（否则上层预算永远是 0）。
func ScopesFor(key *APIKey, project *Project, tenant *Tenant) []QuotaScope {
	out := make([]QuotaScope, 0, 3)
	if key != nil && key.ID != "" {
		out = append(out, QuotaScope{
			Scope: "key:" + key.ID, Label: keyLabel(key),
			RPMLimit: key.RPMLimit, TPMLimit: key.TPMLimit,
			DailyCostLimit: key.DailyCostLimit, MonthlyCostLimit: key.MonthlyCostLimit,
		})
	}
	if project != nil && project.ID != "" {
		out = append(out, QuotaScope{
			Scope: "project:" + project.ID, Label: "project " + project.ID,
			RPMLimit: project.RPMLimit, TPMLimit: project.TPMLimit,
			DailyCostLimit: project.DailyCostLimit, MonthlyCostLimit: project.MonthlyCostLimit,
		})
	}
	if tenant != nil && tenant.ID != "" {
		out = append(out, QuotaScope{
			Scope: "tenant:" + tenant.ID, Label: "tenant " + tenant.ID,
			RPMLimit: tenant.RPMLimit, TPMLimit: tenant.TPMLimit,
			DailyCostLimit: tenant.DailyCostLimit, MonthlyCostLimit: tenant.MonthlyCostLimit,
		})
	}
	return out
}

// scopeLabel 把记账键变成展示名："project:web" → "project web"。
func scopeLabel(scope string) string { return strings.Replace(scope, ":", " ", 1) }

func keyLabel(key *APIKey) string {
	if key.Name != "" {
		return fmt.Sprintf("API key %s (%s)", key.Name, key.KeyPrefix)
	}
	return "API key " + key.KeyPrefix
}

// Denial 一次被拒绝的配额判定。
type Denial struct {
	Code       string
	Message    string
	RetryAfter int // 秒
	// Scope 被拒绝的层级（如 "tenant:acme"）。告警据此把"配额打满"归因到正确的对象；
	// 没有它就只能猜成 key 层。
	Scope string `json:"scope,omitempty"`
	// Label 该层级的展示名，进消息与告警文案。
	Label string `json:"label,omitempty"`
}

func (d *Denial) Error() string { return d.Message }

// Options 限流器参数。
type Options struct {
	// DefaultMaxOutputTokens 请求没写 max_tokens 时,用它对输出 token 做上界估计。
	DefaultMaxOutputTokens int
	// Now 可注入时钟(测试用)。
	Now func() time.Time
}

// Limiter 是数据面的配额引擎。
//
// 语义:
//   - RPM:固定 1 分钟窗口(按墙钟对齐),在请求进入时判定并计数;每层各自计数。
//   - TPM:同一窗口内累计实际 token,在请求结束时结算;每层各自累计。
//   - 成本:日/月两个维度,采用"预扣 + 结算"——请求进入时按 **成本上界** 预扣,
//     结束后用实际成本替换。
//
// 计数器是进程内的。多实例部署时每个实例各自计数(见设计文档"已知限制")。
type Limiter struct {
	mu       sync.Mutex
	opts     Options
	minute   map[string]*minuteBucket // key: scope（"key:k1" / "project:web" / "tenant:acme"）
	buckets  map[string]*costBucket   // key: scope
	reserves int64                    // Reserve 调用次数，用于周期性清理过期桶
}

type minuteBucket struct {
	epoch    int64 // 分钟序号
	requests int
	tokens   int
}

type costBucket struct {
	dayKey   string
	monthKey string
	daily    float64
	monthly  float64
	// pending 是在途请求预扣的成本(按上界),结算时替换为实际值。
	pending float64
}

// Reservation 一次成功预扣的凭据,请求结束后必须 Settle(可 defer)。
type Reservation struct {
	limiter  *Limiter
	scopes   []string // 记账键（每层都记全额）
	estimate float64
	done     bool
}

// ScopeUsage 某一层级的实时用量。
type ScopeUsage struct {
	Scope       string  `json:"scope"`
	RPMUsed     int     `json:"rpm_used"`
	TPMUsed     int     `json:"tpm_used"`
	DailyCost   float64 `json:"daily_cost"`
	MonthlyCost float64 `json:"monthly_cost"`
	PendingCost float64 `json:"pending_cost"`
}

// Snapshot 配额使用情况(给管理 API / 前端展示)。
type Snapshot struct {
	KeyID         string  `json:"key_id"`
	RPMUsed       int     `json:"rpm_used"`
	RPMWindowSecs int     `json:"rpm_window_secs"`
	TPMUsed       int     `json:"tpm_used"`
	DailyCost     float64 `json:"daily_cost"`
	MonthlyCost   float64 `json:"monthly_cost"`
	PendingCost   float64 `json:"pending_cost"`
	// Scopes 各层级用量（key → project → tenant），控制台据此显示"哪一层快满了"。
	// 顶层字段始终是 key 层的值，保持向后兼容。
	Scopes []ScopeUsage `json:"scopes,omitempty"`
}

// NewLimiter 构造限流器。
func NewLimiter(opts Options) *Limiter {
	if opts.DefaultMaxOutputTokens <= 0 {
		opts.DefaultMaxOutputTokens = 4096
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Limiter{
		opts:    opts,
		minute:  map[string]*minuteBucket{},
		buckets: map[string]*costBucket{},
	}
}

// ScopeKeys 返回一次数据面请求要记账的三个层级(旧接口，保留给调用方)。
func ScopeKeys(id *Identity) []string {
	if id == nil {
		return nil
	}
	var out []string
	if id.KeyID != "" {
		out = append(out, "key:"+id.KeyID)
	}
	if id.ProjectID != "" {
		out = append(out, "project:"+id.ProjectID)
	}
	if id.TenantID != "" {
		out = append(out, "tenant:"+id.TenantID)
	}
	return out
}

// scopesForIdentity 取本次请求要判定的层级。
//
// 正常路径由鉴权中间件从 key/project/tenant 三个实体组装（Identity.QuotaScopes）。
// 手工构造 Identity 的调用方（测试、嵌入式用法）没有它，这里退化为旧语义：
// key 的限额作用在 key 层，同时三层都继续记账。
func scopesForIdentity(id *Identity) []QuotaScope {
	if len(id.QuotaScopes) > 0 {
		return id.QuotaScopes
	}
	// 层级顺序与命名复用 ScopeKeys，避免两处各写一份 key → project → tenant。
	names := ScopeKeys(id)
	out := make([]QuotaScope, 0, len(names))
	for _, name := range names {
		s := QuotaScope{Scope: name, Label: scopeLabel(name)}
		if name == "key:"+id.KeyID {
			// 旧字段是 key 层的限额（限额以前只能配在 key 上）。
			s.RPMLimit = id.RPMLimit
			s.TPMLimit = id.TPMLimit
			s.DailyCostLimit = id.DailyCostLimit
			s.MonthlyCostLimit = id.MonthlyCostLimit
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		// 连归属都没有：仍然记账，避免匿名请求绕过成本统计。
		out = append(out, QuotaScope{
			Scope: "anonymous", Label: "anonymous",
			RPMLimit: id.RPMLimit, TPMLimit: id.TPMLimit,
			DailyCostLimit: id.DailyCostLimit, MonthlyCostLimit: id.MonthlyCostLimit,
		})
	}
	return out
}

// Reserve 在请求进入时做全部配额判定并预扣。
//
// 判定顺序：先把每一层都判一遍，全部通过后才统一计数/预扣——避免"上层拒绝但下层已经计数"
// 造成同一分钟里反复被拒后的计数漂移。
func (l *Limiter) Reserve(id *Identity, estCost float64, estTokens int) (*Reservation, *Denial) {
	if id == nil || id.Kind != IdentityAPIKey {
		return &Reservation{limiter: l, done: true}, nil
	}
	now := l.opts.Now()
	secondsToNextMinute := 60 - now.Second()
	scopes := scopesForIdentity(id)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserves++
	l.sweepLocked(now)

	// 1) 判定所有层级的 RPM / TPM / 日成本 / 月成本
	for _, s := range scopes {
		if s.RPMLimit > 0 {
			if b := l.minuteBucketLocked(s.Scope, now); b.requests >= s.RPMLimit {
				return nil, &Denial{
					Code:       ReasonRPM,
					Message:    fmt.Sprintf("rate limit exceeded at %s: %d requests per minute", s.Label, s.RPMLimit),
					RetryAfter: secondsToNextMinute,
					Scope:      s.Scope, Label: s.Label,
				}
			}
		}
		if s.TPMLimit > 0 && estTokens > 0 {
			if b := l.minuteBucketLocked(s.Scope, now); b.tokens+estTokens > s.TPMLimit {
				return nil, &Denial{
					Code:       ReasonTPM,
					Message:    fmt.Sprintf("token rate limit exceeded at %s: %d tokens per minute", s.Label, s.TPMLimit),
					RetryAfter: secondsToNextMinute,
					Scope:      s.Scope, Label: s.Label,
				}
			}
		}
		if s.DailyCostLimit > 0 || s.MonthlyCostLimit > 0 {
			b := l.costBucketLocked(s.Scope, now)
			if s.DailyCostLimit > 0 && b.daily+b.pending+estCost > s.DailyCostLimit {
				return nil, &Denial{
					Code:       ReasonDailyCost,
					Message:    fmt.Sprintf("daily cost budget exceeded at %s (limit %.4f USD)", s.Label, s.DailyCostLimit),
					RetryAfter: secondsUntilNextDay(now),
					Scope:      s.Scope, Label: s.Label,
				}
			}
			if s.MonthlyCostLimit > 0 && b.monthly+b.pending+estCost > s.MonthlyCostLimit {
				return nil, &Denial{
					Code:       ReasonMonthlyCost,
					Message:    fmt.Sprintf("monthly cost budget exceeded at %s (limit %.4f USD)", s.Label, s.MonthlyCostLimit),
					RetryAfter: secondsUntilNextDay(now),
					Scope:      s.Scope, Label: s.Label,
				}
			}
		}
	}

	// 2) 全部通过：计数 + 预扣。每层都记本次请求的全额成本。
	names := make([]string, 0, len(scopes))
	for _, s := range scopes {
		l.minuteBucketLocked(s.Scope, now).requests++
		l.costBucketLocked(s.Scope, now).pending += estCost
		names = append(names, s.Scope)
	}
	return &Reservation{limiter: l, scopes: names, estimate: estCost}, nil
}

// Settle 请求结束后按实际值结算,释放预扣。
func (r *Reservation) Settle(actualTokens int, actualCost float64) {
	if r == nil || r.done || r.limiter == nil {
		return
	}
	l := r.limiter
	now := l.opts.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	r.done = true

	// 释放预扣(必须与 Reserve 对称:每层都记了全额)
	if r.estimate > 0 {
		for _, scope := range r.scopes {
			if b, ok := l.buckets[scope]; ok {
				b.pending -= r.estimate
				if b.pending < 0 {
					b.pending = 0
				}
			}
		}
	}
	// 记实际用量：token 与成本都记到每一层
	if actualTokens > 0 {
		for _, scope := range r.scopes {
			if b := l.minuteBucketLocked(scope, now); b != nil {
				b.tokens += actualTokens
			}
		}
	}
	if actualCost > 0 {
		for _, scope := range r.scopes {
			b := l.costBucketLocked(scope, now)
			b.daily += actualCost
			b.monthly += actualCost
		}
	}
}

// Snapshot 返回某 key 及其项目/租户的当前用量。
func (l *Limiter) Snapshot(keyID string, projectID, tenantID string) Snapshot {
	now := l.opts.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := Snapshot{KeyID: keyID, RPMWindowSecs: 60}

	scopeNames := make([]string, 0, 3)
	if keyID != "" {
		scopeNames = append(scopeNames, "key:"+keyID)
	}
	if projectID != "" {
		scopeNames = append(scopeNames, "project:"+projectID)
	}
	if tenantID != "" {
		scopeNames = append(scopeNames, "tenant:"+tenantID)
	}
	for _, scope := range scopeNames {
		usage := l.scopeUsageLocked(scope, now)
		out.Scopes = append(out.Scopes, usage)
		// 顶层字段 = key 层；没有 key 归属时退化为第一层，保持旧调用方的语义。
		if scope == "key:"+keyID || (keyID == "" && len(out.Scopes) == 1) {
			out.RPMUsed = usage.RPMUsed
			out.TPMUsed = usage.TPMUsed
			out.DailyCost = usage.DailyCost
			out.MonthlyCost = usage.MonthlyCost
			out.PendingCost = usage.PendingCost
		}
	}
	return out
}

func (l *Limiter) scopeUsageLocked(scope string, now time.Time) ScopeUsage {
	usage := ScopeUsage{Scope: scope}
	if b, ok := l.minute[scope]; ok && b.epoch == minuteEpoch(now) {
		usage.RPMUsed = b.requests
		usage.TPMUsed = b.tokens
	}
	if b, ok := l.buckets[scope]; ok {
		usage.DailyCost = b.daily
		usage.MonthlyCost = b.monthly
		usage.PendingCost = b.pending
	}
	return usage
}

// Load 用外部数据(如从 spend 记录聚合出的结果)覆盖计数器,用于重启校准。
func (l *Limiter) Load(scope string, daily, monthly float64, tokens int) {
	now := l.opts.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.costBucketLocked(scope, now)
	if daily > b.daily {
		b.daily = daily
	}
	if monthly > b.monthly {
		b.monthly = monthly
	}
	if tokens > 0 && strings.HasPrefix(scope, "key:") {
		mb := l.minuteBucketLocked(scope, now)
		if mb.tokens < tokens {
			mb.tokens = tokens
		}
	}
}

// sweepLocked 周期性清理过期桶，避免 map 随历史 key/租户无界增长。
//
// 分钟桶：上一分钟之前的直接删。
// 成本桶：只在**跨月**后删——日/月用量同住一个桶，跨天删会把当月累计一起丢掉。
func (l *Limiter) sweepLocked(now time.Time) {
	if l.reserves%1024 != 0 {
		return
	}
	epoch, month := minuteEpoch(now), monthKey(now)
	for key, b := range l.minute {
		if b.epoch < epoch-1 {
			delete(l.minute, key)
		}
	}
	for key, b := range l.buckets {
		if b.monthKey != month {
			delete(l.buckets, key)
		}
	}
}

func (l *Limiter) minuteBucketLocked(scope string, now time.Time) *minuteBucket {
	epoch := minuteEpoch(now)
	b, ok := l.minute[scope]
	if !ok {
		b = &minuteBucket{epoch: epoch}
		l.minute[scope] = b
		return b
	}
	if b.epoch != epoch {
		b.epoch, b.requests, b.tokens = epoch, 0, 0
	}
	return b
}

func (l *Limiter) costBucketLocked(scope string, now time.Time) *costBucket {
	day, month := dayKey(now), monthKey(now)
	b, ok := l.buckets[scope]
	if !ok {
		b = &costBucket{dayKey: day, monthKey: month}
		l.buckets[scope] = b
		return b
	}
	if b.dayKey != day {
		b.dayKey, b.daily, b.pending = day, 0, 0
	}
	if b.monthKey != month {
		b.monthKey, b.monthly = month, 0
	}
	return b
}

func minuteEpoch(t time.Time) int64 { return t.Unix() / 60 }

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }

func secondsUntilNextDay(t time.Time) int {
	next := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Add(24 * time.Hour)
	return int(next.Sub(t).Seconds())
}

// EstimateOutputTokens 从请求里取输出 token 的上界。
func EstimateOutputTokens(req map[string]any, fallback int) int {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := req[key]; ok {
			if n, ok := toInt(v); ok && n > 0 {
				return n
			}
		}
	}
	return fallback
}

// EstimateInputTokens 粗估输入 token:按序列化后的字符数 / 4。
// 只用于成本上界估计,不参与计费(计费用上游返回的真实 usage)。
func EstimateInputTokens(req map[string]any) int {
	raw, err := json.Marshal(req)
	if err != nil {
		return 0
	}
	if len(raw) < 4 {
		return len(raw)
	}
	return len(raw) / 4
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
