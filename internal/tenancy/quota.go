package tenancy

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// 拒绝原因码(会出现在给客户端的错误里,前端也用它做文案)。
const (
	ReasonRPM         = "rpm_exceeded"
	ReasonTPM         = "tpm_exceeded"
	ReasonDailyCost   = "daily_cost_exceeded"
	ReasonMonthlyCost = "monthly_cost_exceeded"
)

// Denial 一次被拒绝的配额判定。
type Denial struct {
	Code       string
	Message    string
	RetryAfter int // 秒
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
//   - RPM:固定 1 分钟窗口(按墙钟对齐),在请求进入时判定并计数。
//   - TPM:同一窗口内累计实际 token,在请求结束时结算。
//   - 成本:日/月两个维度,采用"预扣 + 结算"——请求进入时按 **成本上界** 预扣,
//     结束后用实际成本替换。这样保证不会超支(可能偏保守,提前拒绝)。
//
// 计数器是进程内的。多实例部署时每个实例各自计数(见设计文档"已知限制")。
type Limiter struct {
	mu      sync.Mutex
	opts    Options
	minute  map[string]*minuteBucket // key: apikey id
	buckets map[string]*costBucket   // key: scope key (key:/project:/tenant:)
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
	keyID    string
	scopes   []string
	estimate float64
	done     bool
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

// ScopeKeys 返回一次数据面请求要记账的三个层级。
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

// Reserve 在请求进入时做全部配额判定并预扣。
// estCost 是本次请求的成本上界(由调用方按模型定价与 max_tokens 估算)。
// estTokens 是输出 token 上界,仅用于展示与 TPM 的粗判。
func (l *Limiter) Reserve(id *Identity, estCost float64, estTokens int) (*Reservation, *Denial) {
	if id == nil || id.Kind != IdentityAPIKey {
		return &Reservation{limiter: l, done: true}, nil
	}
	now := l.opts.Now()
	secondsToNextMinute := 60 - now.Second()

	l.mu.Lock()
	defer l.mu.Unlock()

	// 1) RPM
	if id.RPMLimit > 0 {
		b := l.minuteBucketLocked(id.KeyID, now)
		if b.requests >= id.RPMLimit {
			return nil, &Denial{
				Code:       ReasonRPM,
				Message:    fmt.Sprintf("rate limit exceeded: %d requests per minute", id.RPMLimit),
				RetryAfter: secondsToNextMinute,
			}
		}
	}

	// 2) TPM(用当前窗口已用 + 本次上界做保守判定)
	if id.TPMLimit > 0 && estTokens > 0 {
		b := l.minuteBucketLocked(id.KeyID, now)
		if b.tokens+estTokens > id.TPMLimit {
			return nil, &Denial{
				Code:       ReasonTPM,
				Message:    fmt.Sprintf("token rate limit exceeded: %d tokens per minute", id.TPMLimit),
				RetryAfter: secondsToNextMinute,
			}
		}
	}

	// 3) 成本:预扣后不得越过日/月上限
	scopes := ScopeKeys(id)
	if (id.DailyCostLimit > 0 || id.MonthlyCostLimit > 0) && len(scopes) > 0 {
		keyBucket := l.costBucketLocked(scopes[0], now)
		if id.DailyCostLimit > 0 && keyBucket.daily+keyBucket.pending+estCost > id.DailyCostLimit {
			return nil, &Denial{
				Code:    ReasonDailyCost,
				Message: fmt.Sprintf("daily cost budget exceeded (limit %.4f USD)", id.DailyCostLimit),
				// 到次日零点;给个粗略值即可
				RetryAfter: secondsUntilNextDay(now),
			}
		}
		if id.MonthlyCostLimit > 0 && keyBucket.monthly+keyBucket.pending+estCost > id.MonthlyCostLimit {
			return nil, &Denial{
				Code:       ReasonMonthlyCost,
				Message:    fmt.Sprintf("monthly cost budget exceeded (limit %.4f USD)", id.MonthlyCostLimit),
				RetryAfter: secondsUntilNextDay(now),
			}
		}
	}

	// 计数 + 预扣。注意:key/project/tenant 是三个**互相独立**的预算,
	// 每层都要记本次请求的全额成本(平摊会让各层预算被低估)。
	if b := l.minuteBucketLocked(id.KeyID, now); b != nil {
		b.requests++
	}
	for _, scope := range scopes {
		b := l.costBucketLocked(scope, now)
		b.pending += estCost
	}
	return &Reservation{
		limiter:  l,
		keyID:    id.KeyID,
		scopes:   scopes,
		estimate: estCost,
	}, nil
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
	// 记实际用量
	if b := l.minuteBucketLocked(r.keyID, now); b != nil && actualTokens > 0 {
		b.tokens += actualTokens
	}
	if actualCost > 0 {
		for _, scope := range r.scopes {
			b := l.costBucketLocked(scope, now)
			b.daily += actualCost
			b.monthly += actualCost
		}
	}
}

// Snapshot 返回某 key 的当前用量。
func (l *Limiter) Snapshot(keyID string, projectID, tenantID string) Snapshot {
	now := l.opts.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := Snapshot{KeyID: keyID, RPMWindowSecs: 60}
	if b, ok := l.minute[keyID]; ok && b.epoch == minuteEpoch(now) {
		out.RPMUsed = b.requests
		out.TPMUsed = b.tokens
	}
	if b := l.costBucketLocked("key:"+keyID, now); b != nil {
		out.DailyCost = b.daily
		out.MonthlyCost = b.monthly
		out.PendingCost = b.pending
	}
	return out
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
	if tokens > 0 && len(scope) > 4 && scope[:4] == "key:" {
		mb := l.minuteBucketLocked(scope[4:], now)
		if mb.tokens < tokens {
			mb.tokens = tokens
		}
	}
}

func (l *Limiter) minuteBucketLocked(keyID string, now time.Time) *minuteBucket {
	epoch := minuteEpoch(now)
	b, ok := l.minute[keyID]
	if !ok {
		b = &minuteBucket{epoch: epoch}
		l.minute[keyID] = b
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
