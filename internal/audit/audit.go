// Package audit 记录管理面的写操作与敏感动作,回答"谁在什么时候改了什么"。
//
// 设计要点:
//
//   - 存储走既有 store.JSONStore(键前缀 audit/<day>/<unixnano>-<rand>),按天分片,
//     因此按时间范围查询只需扫描区间内的天,不会全表扫描。
//   - 记录路径分两层:HTTP 中间件做**兜底**(任何变更方法都会被记一条),业务处理器
//     再做**语义增强**(记清 action/resource/detail,例如"谁把 gpt-x 的输入价从
//     A 改成 B""谁创建了 key sk-lm-...")。同一次请求只落一条:处理器显式记录后,
//     中间件不再补记,避免重复。
//   - 永远不落密钥材料:detail 经脱敏(api_key/password/token/key_hash/... → "***"),
//     密钥只留 id 与前缀。
//   - 写入异步化(有界队列),队列满时降级为同步写 —— 审计宁可慢一点也不丢。
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/tenancy"
)

// Prefix 审计记录在 JSONStore 里的键前缀。
const Prefix = "audit/"

const (
	OutcomeOK     = "ok"
	OutcomeError  = "error"
	OutcomeDenied = "denied"
)

// Actor 一次操作的主体。Label 是给人看的(邮箱或 master)。
type Actor struct {
	Kind   string `json:"kind"`
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role,omitempty"`
	Label  string `json:"label"`
}

// Entry 一条审计记录。
type Entry struct {
	ID         string         `json:"id"`
	At         time.Time      `json:"at"`
	Actor      Actor          `json:"actor"`
	Action     string         `json:"action"`
	Resource   string         `json:"resource,omitempty"`
	TenantID   string         `json:"tenant_id,omitempty"`
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path,omitempty"`
	Status     int            `json:"status,omitempty"`
	Outcome    string         `json:"outcome"`
	Detail     map[string]any `json:"detail,omitempty"`
	RemoteAddr string         `json:"remote_addr,omitempty"`
	UserAgent  string         `json:"user_agent,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
}

// Options 审计参数。
type Options struct {
	// Retention 保留时长,超过的在写入时顺带清理。<=0 用 90 天。
	Retention time.Duration
	// QueueSize 异步队列长度,<=0 用 1024。
	QueueSize int
	// PruneEvery 每写多少条触发一次清理,<=0 用 500。
	PruneEvery int
	// Now 可注入时钟(测试用)。
	Now func() time.Time
}

// Query 审计查询条件。
type Query struct {
	From     time.Time
	To       time.Time
	Actor    string // 匹配 user_id / email / label / kind
	Action   string // 子串匹配
	Resource string // 子串匹配
	TenantID string // 精确匹配;空 = 不过滤
	Limit    int
	Offset   int
}

// Result 查询结果。
type Result struct {
	Items  []*Entry `json:"items"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// Logger 审计日志器。js 为 nil 时为 no-op(未配置存储的单元测试/本地模式)。
type Logger struct {
	js   store.JSONStore
	opts Options

	ch      chan *Entry
	closing atomic.Bool
	wg      sync.WaitGroup

	mu        sync.Mutex
	writes    int64
	lastPrune time.Time

	// fallbackSync 统计"队列满 → 退化为同步写"的次数。
	// 它应该恒为 0；一旦增长说明审计写入正在拖慢请求，是运维需要知道的信号。
	fallbackSync int64
}

// New 构造并启动后台写协程;js 为 nil 时返回 no-op logger。
func New(js store.JSONStore, opts Options) *Logger {
	if opts.Retention <= 0 {
		opts.Retention = 90 * 24 * time.Hour
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.PruneEvery <= 0 {
		opts.PruneEvery = 500
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	l := &Logger{js: js, opts: opts, ch: make(chan *Entry, opts.QueueSize)}
	if js != nil {
		l.wg.Add(1)
		go l.loop()
	}
	return l
}

// Enabled 是否真的在记录。
func (l *Logger) Enabled() bool { return l != nil && l.js != nil }

// Close 排空队列并停止写协程。
func (l *Logger) Close() {
	if l == nil || l.js == nil {
		return
	}
	if l.closing.Swap(true) {
		return
	}
	close(l.ch)
	l.wg.Wait()
}

func (l *Logger) loop() {
	defer l.wg.Done()
	for e := range l.ch {
		l.write(e)
	}
}

// Record 记一条审计。detail 会先脱敏。
//
// 永不返回错误、永不阻塞调用方超过一次同步写:审计不能把业务请求打挂。
func (l *Logger) Record(ctx context.Context, e *Entry) {
	if !l.Enabled() || e == nil {
		return
	}
	now := l.opts.Now().UTC()
	if e.At.IsZero() {
		e.At = now
	} else {
		e.At = e.At.UTC()
	}
	if e.ID == "" {
		e.ID = newID(e.At)
	}
	if e.Outcome == "" {
		e.Outcome = OutcomeOK
	}
	// 调用方没显式给身份时，从请求上下文里的鉴权身份补齐。
	if e.Actor.Kind == "" {
		e.Actor = actorOf(ctx)
	}
	if e.TenantID == "" {
		e.TenantID = tenantOf(ctx)
	}
	if e.Detail != nil {
		e.Detail = Redact(e.Detail).(map[string]any)
	}
	select {
	case l.ch <- e:
	default:
		// 队列满:同步写,宁可多花一点延迟也不丢审计。
		n := atomic.AddInt64(&l.fallbackSync, 1)
		log.Printf("[audit] write queue full, falling back to synchronous write (count=%d)", n)
		l.write(e)
	}
}

func (l *Logger) write(e *Entry) {
	key := Prefix + e.At.Format("2006-01-02") + "/" + e.ID
	if _, err := l.js.Put(context.Background(), key, e); err != nil {
		log.Printf("[audit] write failed key=%s action=%s: %v", key, e.Action, err)
		return
	}
	l.mu.Lock()
	l.writes++
	due := l.writes%int64(l.opts.PruneEvery) == 0 && l.opts.Now().Sub(l.lastPrune) > time.Hour
	if due {
		l.lastPrune = l.opts.Now()
	}
	l.mu.Unlock()
	if due {
		if n, err := l.Prune(context.Background(), l.opts.Now().Add(-l.opts.Retention)); err != nil {
			log.Printf("[audit] prune failed: %v", err)
		} else if n > 0 {
			log.Printf("[audit] pruned %d entries older than %s", n, l.opts.Retention)
		}
	}
}

// Prune 删除 before 之前(按天)的审计记录,返回删除条数。
func (l *Logger) Prune(ctx context.Context, before time.Time) (int, error) {
	if !l.Enabled() || before.IsZero() {
		return 0, nil
	}
	cutoff := before.UTC().Format("2006-01-02")
	days, err := l.js.List(ctx, Prefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, d := range days {
		rest := strings.TrimPrefix(d.Key, Prefix)
		day := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			day = rest[:i]
		}
		if day >= cutoff { // 键按日期字典序,日期 >= 截止日的一律保留
			continue
		}
		if err := l.js.Delete(ctx, d.Key); err != nil && err != store.ErrNotFound {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// Query 按时间范围 + 条件查询,按下标分页,返回总数。
func (l *Logger) Query(ctx context.Context, q Query) (Result, error) {
	if !l.Enabled() {
		return Result{Items: []*Entry{}}, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	from, to := q.From, q.To
	if to.IsZero() {
		to = l.opts.Now().UTC()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -7)
	}
	if from.After(to) {
		from, to = to, from
	}

	matched := make([]*Entry, 0, 128)
	from = from.UTC()
	to = to.UTC()
	// 按天分片扫描:每片只列当天的键。
	for day := from.Truncate(24 * time.Hour); !day.After(to); day = day.AddDate(0, 0, 1) {
		// 末片可能只到 to 时刻
		docs, err := l.js.List(ctx, Prefix+day.Format("2006-01-02")+"/")
		if err != nil {
			return Result{}, err
		}
		for _, d := range docs {
			var e Entry
			if err := json.Unmarshal(d.Data, &e); err != nil {
				continue
			}
			if e.At.Before(from) || e.At.After(to) {
				continue
			}
			if !matchEntry(&e, q) {
				continue
			}
			matched = append(matched, &e)
		}
	}
	// 最新在前
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].At.Equal(matched[j].At) {
			return matched[i].ID > matched[j].ID
		}
		return matched[i].At.After(matched[j].At)
	})
	total := len(matched)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		matched = nil
	} else {
		matched = matched[offset:]
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	if matched == nil {
		matched = []*Entry{}
	}
	return Result{Items: matched, Total: total, Limit: limit, Offset: offset}, nil
}

func matchEntry(e *Entry, q Query) bool {
	if q.TenantID != "" && e.TenantID != q.TenantID {
		return false
	}
	if q.Action != "" && !strings.Contains(strings.ToLower(e.Action), strings.ToLower(q.Action)) {
		return false
	}
	if q.Resource != "" && !strings.Contains(strings.ToLower(e.Resource), strings.ToLower(q.Resource)) {
		return false
	}
	if q.Actor != "" {
		a := strings.ToLower(q.Actor)
		if !strings.Contains(strings.ToLower(e.Actor.UserID), a) &&
			!strings.Contains(strings.ToLower(e.Actor.Email), a) &&
			!strings.Contains(strings.ToLower(e.Actor.Label), a) &&
			!strings.Contains(strings.ToLower(e.Actor.Kind), a) {
			return false
		}
	}
	return true
}

// ---------- 请求作用域 ----------

type reqAudit struct {
	logger   *Logger
	entry    *Entry
	recorded bool
}

type ctxKey struct{}

// Middleware 给管理面请求挂上审计兜底记录。
//
// 安全方法(GET/HEAD/OPTIONS)不产生兜底记录 —— 它们不是写操作;需要留痕的读取
// 由处理器显式 Record 决定。变更方法末尾若处理器没显式记录,这里补一条通用记录,
// 保证"新增一个写接口就自动有审计"。
func (l *Logger) Middleware(next http.Handler) http.Handler {
	if !l.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ra := &reqAudit{logger: l, entry: &Entry{
			Method:     r.Method,
			Path:       r.URL.Path,
			RemoteAddr: r.RemoteAddr,
			UserAgent:  truncate(r.UserAgent(), 200),
			RequestID:  r.Header.Get("X-Request-ID"),
			Actor:      actorOf(r.Context()),
			TenantID:   tenantOf(r.Context()),
		}}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), ctxKey{}, ra)
		next.ServeHTTP(sw, r.WithContext(ctx))

		if ra.recorded || safeMethod(r.Method) {
			return
		}
		e := ra.entry
		e.Status = sw.status
		e.Outcome = outcomeOf(sw.status)
		e.Action = genericAction(r.Method, r.URL.Path)
		e.Resource = r.URL.Path
		l.Record(ctx, e)
	})
}

// Record 在请求作用域内记一条语义化审计。没有中间件/存储时是 no-op。
//
// detail 里的敏感字段会自动脱敏;调用方不应传入明文密钥(即使传入也不会落库)。
func Record(ctx context.Context, action, resource string, detail map[string]any) {
	ra, ok := ctx.Value(ctxKey{}).(*reqAudit)
	if !ok || ra == nil || ra.recorded {
		return
	}
	ra.recorded = true
	e := ra.entry
	e.Action = action
	if resource != "" {
		e.Resource = resource
	}
	e.Detail = detail
	ra.logger.Record(ctx, e)
}

// RecordOutcome 在请求作用域内记一条审计并显式给出结果(如登录失败 401)。
func RecordOutcome(ctx context.Context, action, resource, outcome string, status int, detail map[string]any) {
	ra, ok := ctx.Value(ctxKey{}).(*reqAudit)
	if !ok || ra == nil || ra.recorded {
		return
	}
	ra.recorded = true
	e := ra.entry
	e.Action = action
	if resource != "" {
		e.Resource = resource
	}
	e.Outcome = outcome
	e.Status = status
	e.Detail = detail
	ra.logger.Record(ctx, e)
}

// WithActor 用给定身份填充请求作用域的 actor(登录成功后需要在同一请求里留痕时用)。
func WithActor(ctx context.Context, identity *tenancy.Identity) context.Context {
	ra, ok := ctx.Value(ctxKey{}).(*reqAudit)
	if !ok || ra == nil {
		return ctx
	}
	ra.entry.Actor = ActorFromIdentity(identity)
	return ctx
}

// truncate 截断字符串。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func genericAction(method, path string) string {
	return strings.ToLower(method) + " " + path
}

func outcomeOf(status int) string {
	switch {
	case status == 0:
		return OutcomeOK
	case status >= 200 && status < 400:
		return OutcomeOK
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return OutcomeDenied
	default:
		return OutcomeError
	}
}

// statusWriter 捕获响应状态码。
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

// Flush 透传(SSE/流式响应用)。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------- 身份 / 脱敏 ----------

// ActorFromIdentity 把鉴权身份转成审计主体。
func ActorFromIdentity(id *tenancy.Identity) Actor {
	if id == nil {
		return Actor{Kind: "anonymous", Label: "anonymous"}
	}
	switch id.Kind {
	case tenancy.IdentityMaster:
		return Actor{Kind: string(id.Kind), Role: id.Role, Label: "master-key"}
	case tenancy.IdentityUser:
		label := id.Email
		if label == "" {
			label = id.UserID
		}
		return Actor{Kind: string(id.Kind), UserID: id.UserID, Email: id.Email, Role: id.Role, Label: label}
	case tenancy.IdentityAPIKey:
		return Actor{Kind: string(id.Kind), Label: id.KeyPrefix}
	default:
		return Actor{Kind: string(id.Kind), Label: string(id.Kind)}
	}
}

func actorOf(ctx context.Context) Actor {
	if id, ok := tenancy.FromContext(ctx); ok {
		return ActorFromIdentity(id)
	}
	return Actor{Kind: "anonymous", Label: "anonymous"}
}

func tenantOf(ctx context.Context) string {
	if id, ok := tenancy.FromContext(ctx); ok {
		return id.TenantID
	}
	return ""
}

// sensitiveKeys 需要脱敏的字段名(子串匹配,已小写)。
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey", "key_hash",
	"authorization", "cookie", "credential", "master_key", "session",
}

// Redact 递归脱敏 map/slice/字符串。返回值类型与入参一致(map[string]any 或 []any 或标量)。
func Redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if isSensitive(k) {
				out[k] = "***"
				continue
			}
			out[k] = Redact(item)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if isSensitive(k) {
				out[k] = "***"
				continue
			}
			out[k] = truncate(item, 512)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, Redact(item))
		}
		return out
	case string:
		return truncate(v, 512)
	default:
		return value
	}
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Diff 计算两个 JSON 兼容对象之间发生变化的顶层字段。
//
// 返回 map[字段]{"from":旧,"to":新}。用于"模型价从 A 改成 B"这类审计要点。
// 两边都是 map 时逐字段比较;值不做深比较,直接序列化后比对。
func Diff(before, after any) map[string]any {
	bm, ok1 := toMap(before)
	am, ok2 := toMap(after)
	if !ok1 || !ok2 {
		return nil
	}
	out := map[string]any{}
	for k, av := range am {
		bv, existed := bm[k]
		if existed && sameJSON(bv, av) {
			continue
		}
		out[k] = map[string]any{"from": Redact(bv), "to": Redact(av)}
	}
	for k, bv := range bm {
		if _, existed := am[k]; !existed {
			out[k] = map[string]any{"from": Redact(bv), "to": nil}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case nil:
		return map[string]any{}, true
	case []byte:
		var out map[string]any
		if err := json.Unmarshal(m, &out); err != nil {
			return nil, false
		}
		return out, true
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false
		}
		return out, true
	}
}

func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func newID(t time.Time) string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", t.UnixNano())
	}
	return fmt.Sprintf("%d-%s", t.UnixNano(), hex.EncodeToString(buf[:]))
}
