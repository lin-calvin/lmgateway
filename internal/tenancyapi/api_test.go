package tenancyapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/alerts"
	"lmgateway/internal/audit"
	"lmgateway/internal/config"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
	"lmgateway/internal/tenancy"
)

type harness struct {
	handler  http.Handler
	repo     *tenancy.Repository
	ts       store.TSStore
	js       store.JSONStore
	auditLog *audit.Logger
	engine   *alerts.Engine
	cutoff   time.Time
	dims     []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	js := mem.NewJSON()
	repo, err := tenancy.NewRepository(ctx, js)
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{})
	auditLog := audit.New(js, audit.Options{})
	t.Cleanup(auditLog.Close)
	engine := alerts.New(ts, js, func() config.AlertsCfg { return config.DefaultAlertsCfg() }, alerts.Options{})
	h := &harness{repo: repo, ts: ts, js: js, auditLog: auditLog, engine: engine, dims: config.EffectiveRollupDimensions(nil)}
	// 与 main.go 一致：审计中间件包在处理器外层（身份已在请求上下文里注入）。
	// 没有它，处理器里的 audit.Record 就是 no-op。
	h.handler = auditLog.Middleware(New(Deps{
		Repo: repo, TS: ts, SessionTTL: time.Hour,
		Limiter:      tenancy.NewLimiter(tenancy.Options{}),
		UsageMaxDays: 90,
		Audit:        auditLog,
		Alerts:       engine,
		RollupDims:   func() []string { return h.dims },
		SpendCutoff:  func(context.Context) time.Time { return h.cutoff },
	}))
	return h
}

// do 发一个带身份的请求（master = 全局管理员）。
func (h *harness) do(t *testing.T, identity *tenancy.Identity, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if identity != nil {
		req = req.WithContext(tenancy.WithIdentity(req.Context(), identity))
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// doJSON 发一个带 JSON body 的请求。
func (h *harness) doJSON(t *testing.T, identity *tenancy.Identity, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(tenancy.WithIdentity(req.Context(), identity))
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func admin() *tenancy.Identity {
	return &tenancy.Identity{Kind: tenancy.IdentityMaster, Role: tenancy.RoleAdmin}
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func TestListPaginationEnvelopeAndScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, id := range []string{"acme", "beta", "gamma"} {
		if err := h.repo.CreateTenant(ctx, &tenancy.Tenant{ID: id, Name: id}); err != nil {
			t.Fatalf("CreateTenant(%s): %v", id, err)
		}
	}

	type envelope struct {
		Items  []tenancy.Tenant `json:"items"`
		Total  int              `json:"total"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}

	// 不传 limit：行为与旧版一致（全量），现有点位不受影响。
	rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/tenants")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	all := decodeBody[envelope](t, rec)
	if all.Total != 3 || len(all.Items) != 3 || all.Limit != 0 {
		t.Fatalf("unpaged = %+v", all)
	}

	// 显式分页：items 被截断，total 仍是全量。
	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/tenants?limit=2")
	paged := decodeBody[envelope](t, rec)
	if len(paged.Items) != 2 || paged.Total != 3 || paged.Limit != 2 {
		t.Fatalf("paged = %+v", paged)
	}
	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/tenants?limit=2&offset=2")
	last := decodeBody[envelope](t, rec)
	if len(last.Items) != 1 || last.Items[0].ID != "gamma" || last.Offset != 2 {
		t.Fatalf("last page = %+v", last)
	}
	// 非法参数明确报错。
	if rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/tenants?limit=-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative limit status = %d", rec.Code)
	}

	// 租户角色只能看到自己的租户；越权 ?tenant= 直接 403。
	scoped := &tenancy.Identity{Kind: tenancy.IdentityUser, Role: tenancy.RoleMember, TenantID: "acme"}
	rec = h.do(t, scoped, http.MethodGet, "/api/tenancy/tenants")
	visible := decodeBody[envelope](t, rec)
	if visible.Total != 1 || visible.Items[0].ID != "acme" {
		t.Fatalf("scoped list = %+v", visible)
	}
	if rec := h.do(t, scoped, http.MethodGet, "/api/tenancy/tenants?tenant=beta"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant status = %d", rec.Code)
	}
}

func TestTenantAndProjectLimitsRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 创建时就带限额
	createTenant := h.doJSON(t, admin(), http.MethodPost, "/api/tenancy/tenants", map[string]any{
		"id": "acme", "name": "Acme", "rpm_limit": 1000, "daily_cost_limit": 5,
	})
	if createTenant.Code != http.StatusCreated {
		t.Fatalf("create tenant status = %d body=%s", createTenant.Code, createTenant.Body.String())
	}
	createProject := h.doJSON(t, admin(), http.MethodPost, "/api/tenancy/projects", map[string]any{
		"id": "web", "tenant_id": "acme", "name": "Web", "rpm_limit": 600, "monthly_cost_limit": 100,
	})
	if createProject.Code != http.StatusCreated {
		t.Fatalf("create project status = %d", createProject.Code)
	}

	// PATCH 能改限额（以前这些字段根本不在 patch 白名单里）
	patch := h.doJSON(t, admin(), http.MethodPatch, "/api/tenancy/tenants/acme", map[string]any{"rpm_limit": 2000})
	if patch.Code != http.StatusOK {
		t.Fatalf("patch tenant status = %d body=%s", patch.Code, patch.Body.String())
	}
	type tenantBody struct {
		RPMLimit         int     `json:"rpm_limit"`
		DailyCostLimit   float64 `json:"daily_cost_limit"`
		MonthlyCostLimit float64 `json:"monthly_cost_limit"`
	}
	rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/tenants/acme")
	tenant := decodeBody[tenantBody](t, rec)
	if tenant.RPMLimit != 2000 || tenant.DailyCostLimit != 5 {
		t.Fatalf("tenant limits after patch = %+v", tenant)
	}
	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/projects/web")
	project := decodeBody[tenantBody](t, rec)
	if project.RPMLimit != 600 || project.MonthlyCostLimit != 100 {
		t.Fatalf("project limits = %+v", project)
	}

	// 负数限额必须 422，而不是被静默接受成"负预算"
	bad := h.doJSON(t, admin(), http.MethodPatch, "/api/tenancy/tenants/acme", map[string]any{"daily_cost_limit": -1})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative limit status = %d, want 422", bad.Code)
	}

	// key 配额接口要能看到 project/tenant 两层限额，否则"key 没超但上面超了"无法解释
	key := &tenancy.APIKey{TenantID: "acme", ProjectID: "web", Name: "prod", RPMLimit: 10}
	if _, err := h.repo.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	type quotaBody struct {
		Limits struct {
			RPMLimit int `json:"rpm_limit"`
		} `json:"limits"`
		ScopeLimits []struct {
			Scope string `json:"scope"`
		} `json:"scope_limits"`
	}
	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/keys/"+key.ID+"/quota")
	quota := decodeBody[quotaBody](t, rec)
	if quota.Limits.RPMLimit != 10 {
		t.Fatalf("key-level limits missing: %+v", quota)
	}
	if len(quota.ScopeLimits) != 2 {
		t.Fatalf("key quota must expose project/tenant layers, got %+v", quota.ScopeLimits)
	}

	// 审计里要能看出限额被调过
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		res, err := h.auditLog.Query(ctx, audit.Query{Action: "tenant.update", Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total > 0 {
			if res.Items[0].Detail == nil {
				t.Fatal("tenant.update audit has no detail")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tenant.update was not audited")
}

func TestKeysListDoesNotAliasStoredSlices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.repo.CreateTenant(ctx, &tenancy.Tenant{ID: "acme", Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateProject(ctx, &tenancy.Project{ID: "web", TenantID: "acme", Name: "Web"}); err != nil {
		t.Fatal(err)
	}
	key := &tenancy.APIKey{TenantID: "acme", ProjectID: "web", Name: "k", AllowedModels: []string{"gpt-x"}}
	if _, err := h.repo.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}

	type envelope struct {
		Items []tenancy.APIKey `json:"items"`
	}
	rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/keys")
	got := decodeBody[envelope](t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v", got)
	}
	// 篡改响应里的切片，不能影响仓储内部状态。
	got.Items[0].AllowedModels[0] = "hacked"

	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/keys")
	again := decodeBody[envelope](t, rec)
	if again.Items[0].AllowedModels[0] != "gpt-x" {
		t.Fatalf("repository state was mutated through response slice: %+v", again.Items[0].AllowedModels)
	}
}

func TestUsageMergesRollupHistory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -30)
	h.cutoff = now.AddDate(0, 0, -7)

	// 历史（已被裁剪，只存在于日汇总）
	if err := h.ts.Append(ctx, &store.TSRecord{
		Ts: now.AddDate(0, 0, -10), Stream: store.StreamSpendDaily,
		Tags:   map[string]string{"day": now.AddDate(0, 0, -10).Format("2006-01-02"), "tenant": "acme", "provider": "openai", "model": "gpt-x", "stream": "false", "project": "web", "key": "k1"},
		Fields: map[string]any{"requests": float64(4), "total_tokens": float64(400), "prompt_tokens": float64(300), "cost": 1.0},
	}); err != nil {
		t.Fatal(err)
	}
	// 近期（raw）
	if err := h.ts.Append(ctx, &store.TSRecord{
		Ts: now.Add(-time.Hour), Stream: store.StreamSpend,
		Tags:   map[string]string{"tenant": "acme", "provider": "openai", "model": "gpt-x", "stream": "false", "project": "web", "key": "k1"},
		Fields: map[string]any{"total_tokens": float64(100), "prompt_tokens": float64(80), "cost": 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	type row struct {
		Group        string  `json:"group"`
		Requests     float64 `json:"requests"`
		TotalTokens  float64 `json:"total_tokens"`
		CachedTokens float64 `json:"cached_tokens"`
		Cost         float64 `json:"cost"`
	}
	type resp struct {
		GroupBy     string    `json:"group_by"`
		Source      string    `json:"source"`
		CoveredFrom time.Time `json:"covered_from"`
		Items       []row     `json:"items"`
	}

	target := "/api/tenancy/usage?group_by=tenant&from=" + from.Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	rec := h.do(t, admin(), http.MethodGet, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeBody[resp](t, rec)
	if out.Source != "mixed" {
		t.Fatalf("source = %q, want mixed", out.Source)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	item := out.Items[0]
	if item.Group != "acme" || item.Requests != 5 || item.TotalTokens != 500 || item.Cost != 1.5 {
		t.Fatalf("merged row = %+v (want requests=5 tokens=500 cost=1.5)", item)
	}
	// raw 的 requests 记 1，daily 记 4：合计正好 5，说明两侧没有重叠。
	if out.CoveredFrom.IsZero() {
		t.Fatal("covered_from must be reported when daily data is used")
	}

	// 租户过滤仍然生效（历史行也要能被归属）。
	scoped := &tenancy.Identity{Kind: tenancy.IdentityUser, Role: tenancy.RoleMember, TenantID: "beta"}
	rec = h.do(t, scoped, http.MethodGet, target)
	if got := decodeBody[resp](t, rec); len(got.Items) != 0 {
		t.Fatalf("other tenant must not see acme usage: %+v", got.Items)
	}
}

func TestUsageReportsDegradedHistoryExplicitly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -30)
	h.cutoff = now.AddDate(0, 0, -7)
	// 模拟"历史汇总行没有租户维度"（旧版本 rollup 产出的数据 / 维度未启用）。
	h.dims = []string{"provider", "model", "stream"}
	if err := h.ts.Append(ctx, &store.TSRecord{
		Ts: now.AddDate(0, 0, -10), Stream: store.StreamSpendDaily,
		Tags:   map[string]string{"day": "x", "provider": "openai"},
		Fields: map[string]any{"requests": float64(2), "cost": 0.7},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Truncated      bool   `json:"truncated"`
		DegradedReason string `json:"degraded_reason"`
	}
	target := "/api/tenancy/usage?group_by=tenant&from=" + from.Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	rec := h.do(t, admin(), http.MethodGet, target)
	out := decodeBody[resp](t, rec)
	if !out.Truncated || out.DegradedReason == "" {
		t.Fatalf("expected explicit degradation, got %+v", out)
	}
}

func TestAuditEndpointIsScopedAndPaginated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.auditLog.Record(ctx, &audit.Entry{Action: "apikey.create", Resource: "apikey/k1", TenantID: "acme"})
	h.auditLog.Record(ctx, &audit.Entry{Action: "model.patch", Resource: "model/gpt-x"})
	h.auditLog.Record(ctx, &audit.Entry{Action: "apikey.rotate", Resource: "apikey/k2", TenantID: "beta"})

	type envelope struct {
		Items []audit.Entry `json:"items"`
		Total int           `json:"total"`
	}
	// 管理员看全部。
	deadline := time.Now().Add(2 * time.Second)
	var all envelope
	for time.Now().Before(deadline) {
		rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/audit?limit=10")
		all = decodeBody[envelope](t, rec)
		if all.Total == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if all.Total != 3 {
		t.Fatalf("audit total = %d, want 3", all.Total)
	}
	// 分页。
	rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/audit?limit=2&offset=0")
	pageOut := decodeBody[envelope](t, rec)
	if len(pageOut.Items) != 2 || pageOut.Total != 3 {
		t.Fatalf("audit page = %+v", pageOut)
	}
	// 租户身份只看本租户。
	scoped := &tenancy.Identity{Kind: tenancy.IdentityUser, Role: tenancy.RoleMember, TenantID: "acme"}
	rec = h.do(t, scoped, http.MethodGet, "/api/tenancy/audit")
	scopedOut := decodeBody[envelope](t, rec)
	if scopedOut.Total != 1 || scopedOut.Items[0].TenantID != "acme" {
		t.Fatalf("scoped audit = %+v", scopedOut)
	}
	// action 过滤。
	rec = h.do(t, admin(), http.MethodGet, "/api/tenancy/audit?action=model.patch")
	filtered := decodeBody[envelope](t, rec)
	if filtered.Total != 1 || filtered.Items[0].Resource != "model/gpt-x" {
		t.Fatalf("filtered audit = %+v", filtered)
	}
}

func TestAlertsEndpointExposesFiringAlerts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		if err := h.ts.Append(ctx, &store.TSRecord{
			Ts: now, Stream: store.StreamRequestError,
			Tags: map[string]string{
				"tenant": "acme", "project": "web", "key": "k1",
				"kind": "policy", "class": "rate_limit", "reason": "daily_cost_exceeded",
			},
			Fields: map[string]any{"status_code": float64(429)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	type envelope struct {
		Items []alerts.Alert `json:"items"`
		Total int            `json:"total"`
	}
	rec := h.do(t, admin(), http.MethodGet, "/api/tenancy/alerts?state=firing")
	out := decodeBody[envelope](t, rec)
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("alerts = %+v", out)
	}
	if out.Items[0].Rule != alerts.RuleQuotaExhausted || out.Items[0].Severity != alerts.SeverityWarning {
		t.Fatalf("alert = %+v", out.Items[0])
	}
	if out.Items[0].Value != 12 || out.Items[0].Threshold != 10 {
		t.Fatalf("alert thresholds = %+v", out.Items[0])
	}
	if out.Items[0].KeyID != "k1" || out.Items[0].TenantID != "acme" {
		t.Fatalf("alert scope = %+v", out.Items[0])
	}

	// 别的租户看不到 acme 的告警。
	scoped := &tenancy.Identity{Kind: tenancy.IdentityUser, Role: tenancy.RoleMember, TenantID: "beta"}
	rec = h.do(t, scoped, http.MethodGet, "/api/tenancy/alerts")
	if got := decodeBody[envelope](t, rec); got.Total != 0 {
		t.Fatalf("cross-tenant alerts leaked: %+v", got)
	}
}
