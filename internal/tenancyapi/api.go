// Package tenancyapi 暴露多租户管理 API,供独立前端 SPA 与 lmgcli 使用。
//
// 挂载点: /api/tenancy/*（由 main 包套上 tenancy.ScopeAdmin 鉴权中间件）。
//
//	POST   /api/tenancy/bootstrap              首次初始化管理员(仅当无任何用户)
//	POST   /api/tenancy/login                  登录 → 会话 token
//	POST   /api/tenancy/logout                 登出
//	GET    /api/tenancy/me                     当前身份
//	GET    /api/tenancy/overview               概览计数
//	GET    /api/tenancy/tenants                列表(自动按身份收窄)
//	POST   /api/tenancy/tenants                新建
//	GET    /api/tenancy/tenants/{id}           详情
//	PATCH  /api/tenancy/tenants/{id}           局部更新
//	DELETE /api/tenancy/tenants/{id}           删除
//	... projects / users / keys 同构
//	POST   /api/tenancy/keys/{id}/rotate       轮换密钥(明文只返回一次)
//	GET    /api/tenancy/keys/{id}/quota        当前配额用量快照
//	POST   /api/tenancy/users/{id}/password    改密
//	POST   /api/tenancy/users/{id}/sessions/revoke  撤销该用户全部会话
//	GET    /api/tenancy/usage                  用量汇总(?group_by=tenant|project|key)
//	GET    /api/tenancy/audit                  审计日志(谁改了什么,分页)
//	GET    /api/tenancy/alerts                 告警列表(配额打满/上游大面积失败)
package tenancyapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/alerts"
	"lmgateway/internal/audit"
	"lmgateway/internal/spendagg"
	"lmgateway/internal/store"
	"lmgateway/internal/tenancy"
)

// Deps 依赖注入。
type Deps struct {
	Repo         *tenancy.Repository
	Auth         *tenancy.Authenticator
	Limiter      *tenancy.Limiter
	TS           store.TSStore
	SessionTTL   time.Duration
	UsageMaxDays int
	// Models 返回可授权的模型名清单(仅名字)。租户角色读不到配置 API,
	// 但创建密钥时需要勾选"模型权限",因此单独提供一个最小暴露面的端点。
	Models func() []string
	// Display 返回控制台展示设置(币种 + 汇率)。
	// 汇率必须由服务端下发:写死在前端会导致多端不一致、改汇率要重新发版。
	// 它不含任何租户数据,但每个角色格式化金额都要用,所以经这里而不是
	// 只对全局管理员开放的 /api/config/settings/*。
	Display func() DisplaySettings
	// SpendCutoff 返回 spend 原始流水被 rollup 裁剪的时间边界(watermark)。
	// 用量查询据此把 raw 与 spend_daily 拼接:没有它就只能看最近 raw_retention_days。
	SpendCutoff func(context.Context) time.Time
	// RollupDims 返回 spend_daily 实际带有的维度(含强制维度)。
	// 用量页据此判断某个 group_by 能否由历史汇总回答。
	RollupDims func() []string
	// Audit 审计日志器。nil = 不记录(仅测试/本地模式)。
	Audit *audit.Logger
	// Alerts 告警引擎。nil = 不提供告警查询端点。
	Alerts *alerts.Engine
}

// DisplaySettings 控制台展示设置(服务端下发)。
type DisplaySettings struct {
	Currency string  `json:"currency"`
	USDToCNY float64 `json:"usd_to_cny"`
}

type api struct {
	deps Deps
}

// New 组装路由。返回值不含鉴权中间件(由调用方按 ScopeAdmin 包裹)。
func New(deps Deps) http.Handler {
	if deps.SessionTTL <= 0 {
		deps.SessionTTL = 12 * time.Hour
	}
	if deps.UsageMaxDays <= 0 {
		deps.UsageMaxDays = 90
	}
	a := &api{deps: deps}
	mux := http.NewServeMux()

	// 认证(不需要既有会话,但 bootstrap 只允许在空库时使用)
	mux.HandleFunc("POST /api/tenancy/bootstrap", a.handleBootstrap)
	mux.HandleFunc("POST /api/tenancy/login", a.handleLogin)
	mux.HandleFunc("POST /api/tenancy/logout", a.handleLogout)
	mux.HandleFunc("GET /api/tenancy/me", a.handleMe)
	mux.HandleFunc("GET /api/tenancy/overview", a.handleOverview)
	mux.HandleFunc("GET /api/tenancy/models", a.handleModelCatalog)
	mux.HandleFunc("GET /api/tenancy/display", a.handleDisplay)

	// 资源 CRUD(表驱动,避免路径变量歧义)
	for _, resource := range []string{"tenants", "projects", "users", "keys"} {
		mux.HandleFunc("GET /api/tenancy/"+resource, a.list(resource))
		mux.HandleFunc("POST /api/tenancy/"+resource, a.create(resource))
		mux.HandleFunc("GET /api/tenancy/"+resource+"/{id}", a.get(resource))
		mux.HandleFunc("PATCH /api/tenancy/"+resource+"/{id}", a.update(resource))
		mux.HandleFunc("DELETE /api/tenancy/"+resource+"/{id}", a.remove(resource))
	}
	mux.HandleFunc("POST /api/tenancy/keys/{id}/rotate", a.handleRotateKey)
	mux.HandleFunc("GET /api/tenancy/keys/{id}/quota", a.handleKeyQuota)
	mux.HandleFunc("POST /api/tenancy/users/{id}/password", a.handleSetPassword)
	mux.HandleFunc("POST /api/tenancy/users/{id}/sessions/revoke", a.handleRevokeSessions)
	mux.HandleFunc("GET /api/tenancy/usage", a.handleUsage)
	mux.HandleFunc("GET /api/tenancy/audit", a.handleAudit)
	mux.HandleFunc("GET /api/tenancy/alerts", a.handleAlerts)

	return mux
}

// ---------- 认证 ----------

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type bootstrapRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name,omitempty"`
}

func (a *api) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if a.deps.Repo.HasAnyUser() {
		writeError(w, http.StatusConflict, "bootstrap already completed; use login")
		return
	}
	var req bootstrapRequest
	if !decode(w, r, &req) {
		return
	}
	user := &tenancy.User{Email: req.Email, Name: req.Name, Role: tenancy.RoleAdmin, Status: tenancy.StatusActive}
	if err := a.deps.Repo.CreateUser(r.Context(), user, req.Password); err != nil {
		writeRepoError(w, err)
		return
	}
	ctx := audit.WithActor(r.Context(), &tenancy.Identity{
		Kind: tenancy.IdentityUser, UserID: user.ID, Email: user.Email, Role: user.Role, TenantID: user.TenantID,
	})
	audit.RecordOutcome(ctx, "auth.bootstrap", "user/"+user.ID, audit.OutcomeOK, http.StatusOK,
		map[string]any{"email": user.Email, "role": user.Role})
	a.issueSession(w, r, user)
}

func (a *api) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	user, ok := a.deps.Repo.LookupUserByEmail(req.Email)
	if !ok || user.Status != tenancy.StatusActive || !tenancy.VerifyPassword(user.PasswordHash, req.Password) {
		// 登录失败也要留痕(暴力破解排查靠它);但记录里只放尝试的邮箱。
		audit.RecordOutcome(r.Context(), "auth.login", "user/"+req.Email, audit.OutcomeDenied,
			http.StatusUnauthorized, map[string]any{"email": req.Email, "result": "invalid credentials"})
		// 不区分"用户不存在"与"密码错误",避免账号枚举。
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if user.TenantID != "" {
		if tenant, ok := a.deps.Repo.GetTenant(user.TenantID); !ok || tenant.Status != tenancy.StatusActive {
			writeError(w, http.StatusForbidden, "tenant is disabled")
			return
		}
	}
	a.deps.Repo.MarkLogin(r.Context(), user.ID)
	loginCtx := audit.WithActor(r.Context(), &tenancy.Identity{
		Kind: tenancy.IdentityUser, UserID: user.ID, Email: user.Email, Role: user.Role, TenantID: user.TenantID,
	})
	audit.RecordOutcome(loginCtx, "auth.login", "user/"+user.ID, audit.OutcomeOK, http.StatusOK,
		map[string]any{"email": user.Email, "role": user.Role, "tenant_id": user.TenantID})
	a.issueSession(w, r, user)
}

func (a *api) issueSession(w http.ResponseWriter, r *http.Request, user *tenancy.User) {
	token, session, err := a.deps.Repo.CreateSession(r.Context(), user.ID, a.deps.SessionTTL)
	if err != nil {
		writeRepoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": session.ExpiresAt,
		"user": map[string]any{
			"id": user.ID, "email": user.Email, "name": user.Name,
			"role": user.Role, "tenant_id": user.TenantID,
		},
	})
}

func (a *api) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := tenancy.ExtractToken(r)
	if tenancy.LooksLikeSession(token) {
		_ = a.deps.Repo.DeleteSession(r.Context(), tenancy.HashSecret(token))
	}
	if identity, ok := tenancy.FromContext(r.Context()); ok && identity != nil {
		audit.RecordOutcome(r.Context(), "auth.logout", "user/"+identity.UserID, audit.OutcomeOK,
			http.StatusOK, map[string]any{"email": identity.Email})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) handleMe(w http.ResponseWriter, r *http.Request) {
	identity, _ := tenancy.FromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": identity})
}

func (a *api) handleOverview(w http.ResponseWriter, r *http.Request) {
	identity, _ := tenancy.FromContext(r.Context())
	scope := identity.TenantScope()
	stats := a.deps.Repo.Stats()
	if scope != "" {
		stats["tenants"] = 1
		stats["projects"] = len(a.deps.Repo.ListProjects(scope))
		stats["users"] = len(a.deps.Repo.ListUsers(scope))
		stats["keys"] = len(a.deps.Repo.ListAPIKeys(scope, ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": stats})
}

// handleModelCatalog 返回可授权的模型名清单,供前端在"模型权限"里做选择。
//
// 只暴露 **名字**：provider 端点、上游模型名、定价都属于网关配置,
// 租户角色不应该看到(它们走 /api/*,而那里只对全局管理员开放)。
// 模型名本身不是秘密——持数据面密钥的客户端本来就能从 /v1/models 看到。
func (a *api) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireIdentity(w, r); !ok {
		return
	}
	var names []string
	if a.deps.Models != nil {
		names = a.deps.Models()
	}
	seen := map[string]bool{}
	items := []map[string]any{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		items = append(items, map[string]any{"id": name})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleDisplay 下发控制台展示设置(币种 + 汇率)。
//
// 要点:汇率是**服务端**的事实来源。所有已登录角色都能读(格式化金额要用),
// 但这里只返回两个展示字段,不含任何租户数据或网关配置。
func (a *api) handleDisplay(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireIdentity(w, r); !ok {
		return
	}
	settings := DisplaySettings{Currency: "USD", USDToCNY: 7.2}
	if a.deps.Display != nil {
		settings = a.deps.Display()
	}
	if settings.Currency == "" {
		settings.Currency = "USD"
	}
	if settings.USDToCNY <= 0 {
		settings.USDToCNY = 7.2
	}
	writeJSON(w, http.StatusOK, settings)
}

// ---------- 列表 ----------

// pageParams 列表分页参数。limit=0 表示不分页（保持既有客户端行为不变），
// 只有显式传 limit 才截断。
type pageParams struct {
	limit  int
	offset int
}

const maxPageLimit = 1000

func parsePage(r *http.Request) (pageParams, error) {
	var p pageParams
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, errors.New("limit must be a non-negative integer")
		}
		if n > maxPageLimit {
			n = maxPageLimit
		}
		p.limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, errors.New("offset must be a non-negative integer")
		}
		p.offset = n
	}
	return p, nil
}

// paginate 在已排序的内存列表上做 offset/limit 切片。
func paginate[T any](items []T, p pageParams) []T {
	if p.offset > 0 {
		if p.offset >= len(items) {
			return []T{}
		}
		items = items[p.offset:]
	}
	if p.limit > 0 && len(items) > p.limit {
		items = items[:p.limit]
	}
	if items == nil {
		return []T{}
	}
	return items
}

// listEnvelope 统一列表响应：items 保持原样，追加 total/limit/offset 供分页使用。
func listEnvelope[T any](items []T, p pageParams) map[string]any {
	return map[string]any{
		"items":  paginate(items, p),
		"total":  len(items),
		"limit":  p.limit,
		"offset": p.offset,
	}
}

func (a *api) list(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
			return
		}
		page, err := parsePage(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		tenantFilter := identity.TenantScope()
		// 允许管理员用 ?tenant= 过滤
		if q := r.URL.Query().Get("tenant"); q != "" {
			if !identity.CanReadTenant(q) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			tenantFilter = q
		}
		projectFilter := r.URL.Query().Get("project")

		switch resource {
		case "tenants":
			writeJSON(w, http.StatusOK, listEnvelope(a.visibleTenants(identity), page))
		case "projects":
			writeJSON(w, http.StatusOK, listEnvelope(a.deps.Repo.ListProjects(tenantFilter), page))
		case "users":
			writeJSON(w, http.StatusOK, listEnvelope(a.deps.Repo.ListUsers(tenantFilter), page))
		case "keys":
			writeJSON(w, http.StatusOK, listEnvelope(a.deps.Repo.ListAPIKeys(tenantFilter, projectFilter), page))
		default:
			writeError(w, http.StatusNotFound, "unknown resource")
		}
	}
}

func (a *api) visibleTenants(identity *tenancy.Identity) []*tenancy.Tenant {
	if identity.IsAdmin() {
		return a.deps.Repo.ListTenants()
	}
	if tenant, ok := a.deps.Repo.GetTenant(identity.TenantID); ok {
		return []*tenancy.Tenant{tenant}
	}
	return []*tenancy.Tenant{}
}

// ---------- 读取/写入 ----------

func (a *api) get(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		switch resource {
		case "tenants":
			tenant, ok := a.deps.Repo.GetTenant(id)
			if !ok || !identity.CanReadTenant(id) {
				writeNotFound(w)
				return
			}
			writeJSON(w, http.StatusOK, tenant)
		case "projects":
			project, ok := a.deps.Repo.GetProject(id)
			if !ok || !identity.CanReadTenant(project.TenantID) {
				writeNotFound(w)
				return
			}
			writeJSON(w, http.StatusOK, project)
		case "users":
			user, ok := a.deps.Repo.LookupUser(id)
			if !ok || (user.TenantID != "" && !identity.CanReadTenant(user.TenantID)) {
				writeNotFound(w)
				return
			}
			user.PasswordHash = ""
			writeJSON(w, http.StatusOK, user)
		case "keys":
			key, ok := a.deps.Repo.GetAPIKey(id)
			if !ok || !identity.CanReadTenant(key.TenantID) {
				writeNotFound(w)
				return
			}
			key.KeyHash = ""
			writeJSON(w, http.StatusOK, key)
		}
	}
}

func (a *api) create(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		switch resource {
		case "tenants":
			if !identity.IsAdmin() {
				writeError(w, http.StatusForbidden, "only an administrator can create tenants")
				return
			}
			var tenant tenancy.Tenant
			if !decode(w, r, &tenant) {
				return
			}
			if err := a.deps.Repo.CreateTenant(ctx, &tenant); err != nil {
				writeRepoError(w, err)
				return
			}
			audit.Record(ctx, "tenant.create", "tenant/"+tenant.ID, withLimits(map[string]any{
				"tenant_id": tenant.ID, "name": tenant.Name, "status": tenant.Status,
			}, tenant.RPMLimit, tenant.TPMLimit, tenant.DailyCostLimit, tenant.MonthlyCostLimit))
			writeJSON(w, http.StatusCreated, tenant)
		case "projects":
			var project tenancy.Project
			if !decode(w, r, &project) {
				return
			}
			if !identity.CanManageTenant(project.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			if err := a.deps.Repo.CreateProject(ctx, &project); err != nil {
				writeRepoError(w, err)
				return
			}
			audit.Record(ctx, "project.create", "project/"+project.ID, withLimits(map[string]any{
				"project_id": project.ID, "tenant_id": project.TenantID, "name": project.Name,
			}, project.RPMLimit, project.TPMLimit, project.DailyCostLimit, project.MonthlyCostLimit))
			writeJSON(w, http.StatusCreated, project)
		case "users":
			var body struct {
				Email    string `json:"email"`
				Name     string `json:"name"`
				Password string `json:"password"`
				Role     string `json:"role"`
				TenantID string `json:"tenant_id"`
			}
			if !decode(w, r, &body) {
				return
			}
			if body.Role == "" {
				body.Role = tenancy.RoleMember
			}
			// 只有全局管理员能造管理员;租户管理员只能造本租户的成员。
			if body.Role == tenancy.RoleAdmin {
				if !identity.IsAdmin() {
					writeError(w, http.StatusForbidden, "only an administrator can create administrators")
					return
				}
			} else {
				if !identity.CanManageTenant(body.TenantID) {
					writeError(w, http.StatusForbidden, "forbidden")
					return
				}
				if !identity.IsAdmin() && body.Role == tenancy.RoleTenantAdmin && identity.Role != tenancy.RoleTenantAdmin {
					writeError(w, http.StatusForbidden, "forbidden")
					return
				}
			}
			user := &tenancy.User{Email: body.Email, Name: body.Name, Role: body.Role, TenantID: body.TenantID, Status: tenancy.StatusActive}
			if err := a.deps.Repo.CreateUser(ctx, user, body.Password); err != nil {
				writeRepoError(w, err)
				return
			}
			user.PasswordHash = ""
			audit.Record(ctx, "user.create", "user/"+user.ID,
				map[string]any{"user_id": user.ID, "email": user.Email, "role": user.Role, "tenant_id": user.TenantID})
			writeJSON(w, http.StatusCreated, user)
		case "keys":
			var key tenancy.APIKey
			if !decode(w, r, &key) {
				return
			}
			if !identity.CanManageTenant(key.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			if identity.Kind == tenancy.IdentityUser {
				key.CreatedBy = identity.UserID
			}
			plaintext, err := a.deps.Repo.CreateAPIKey(ctx, &key)
			if err != nil {
				writeRepoError(w, err)
				return
			}
			// 审计只记 id 与前缀：明文密钥与 hash 绝不落审计。
			audit.Record(ctx, "apikey.create", "apikey/"+key.ID, map[string]any{
				"key_id": key.ID, "key_prefix": key.KeyPrefix, "name": key.Name,
				"tenant_id": key.TenantID, "project_id": key.ProjectID, "created_by": key.CreatedBy,
				"allowed_models": key.AllowedModels, "denied_models": key.DeniedModels,
				"rpm_limit": key.RPMLimit, "tpm_limit": key.TPMLimit,
				"daily_cost_limit": key.DailyCostLimit, "monthly_cost_limit": key.MonthlyCostLimit,
			})
			// 明文 key 只在这里出现一次。
			key.KeyHash = ""
			writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": plaintext})
		}
	}
}

func (a *api) update(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var patch map[string]any
		if !decode(w, r, &patch) {
			return
		}
		ctx := r.Context()
		switch resource {
		case "tenants":
			if !identity.CanManageTenant(id) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			tenant, err := a.deps.Repo.UpdateTenant(ctx, id, func(t *tenancy.Tenant) {
				applyString(patch, "name", &t.Name)
				applyString(patch, "note", &t.Note)
				applyString(patch, "status", &t.Status)
				applyInt(patch, "rpm_limit", &t.RPMLimit)
				applyInt(patch, "tpm_limit", &t.TPMLimit)
				applyFloat(patch, "daily_cost_limit", &t.DailyCostLimit)
				applyFloat(patch, "monthly_cost_limit", &t.MonthlyCostLimit)
			})
			if err == nil {
				audit.Record(ctx, "tenant.update", "tenant/"+id, map[string]any{"patch": patch})
			}
			respond(w, tenant, err)
		case "projects":
			project, ok := a.deps.Repo.GetProject(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(project.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			updated, err := a.deps.Repo.UpdateProject(ctx, id, func(p *tenancy.Project) {
				applyString(patch, "name", &p.Name)
				applyString(patch, "status", &p.Status)
				applyInt(patch, "rpm_limit", &p.RPMLimit)
				applyInt(patch, "tpm_limit", &p.TPMLimit)
				applyFloat(patch, "daily_cost_limit", &p.DailyCostLimit)
				applyFloat(patch, "monthly_cost_limit", &p.MonthlyCostLimit)
			})
			if err == nil {
				audit.Record(ctx, "project.update", "project/"+id, map[string]any{"patch": patch})
			}
			respond(w, updated, err)
		case "users":
			user, ok := a.deps.Repo.LookupUser(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(user.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			updated, err := a.deps.Repo.UpdateUser(ctx, id, func(u *tenancy.User) {
				applyString(patch, "name", &u.Name)
				applyString(patch, "status", &u.Status)
				applyString(patch, "role", &u.Role)
			})
			if err != nil {
				writeRepoError(w, err)
				return
			}
			if updated.Status != tenancy.StatusActive {
				_ = a.deps.Repo.DeleteUserSessions(ctx, id) // 禁用即踢下线
			}
			updated.PasswordHash = ""
			audit.Record(ctx, "user.update", "user/"+id, map[string]any{
				"patch": patch, "status": updated.Status, "role": updated.Role, "tenant_id": updated.TenantID,
			})
			writeJSON(w, http.StatusOK, updated)
		case "keys":
			key, ok := a.deps.Repo.GetAPIKey(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(key.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			updated, err := a.deps.Repo.UpdateAPIKey(ctx, id, func(k *tenancy.APIKey) {
				applyString(patch, "name", &k.Name)
				applyString(patch, "status", &k.Status)
				applyStringSlice(patch, "allowed_models", &k.AllowedModels)
				applyStringSlice(patch, "denied_models", &k.DeniedModels)
				applyInt(patch, "rpm_limit", &k.RPMLimit)
				applyInt(patch, "tpm_limit", &k.TPMLimit)
				applyFloat(patch, "daily_cost_limit", &k.DailyCostLimit)
				applyFloat(patch, "monthly_cost_limit", &k.MonthlyCostLimit)
			})
			if err != nil {
				writeRepoError(w, err)
				return
			}
			updated.KeyHash = ""
			audit.Record(ctx, "apikey.update", "apikey/"+id, map[string]any{
				"patch": patch, "key_prefix": updated.KeyPrefix,
				"tenant_id": updated.TenantID, "project_id": updated.ProjectID,
			})
			writeJSON(w, http.StatusOK, updated)
		}
	}
}

func (a *api) remove(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		ctx := r.Context()
		var err error
		switch resource {
		case "tenants":
			if !identity.IsAdmin() {
				writeError(w, http.StatusForbidden, "only an administrator can delete tenants")
				return
			}
			err = a.deps.Repo.DeleteTenant(ctx, id)
		case "projects":
			project, ok := a.deps.Repo.GetProject(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(project.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			err = a.deps.Repo.DeleteProject(ctx, id)
		case "users":
			user, ok := a.deps.Repo.LookupUser(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(user.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			err = a.deps.Repo.DeleteUser(ctx, id)
		case "keys":
			key, ok := a.deps.Repo.GetAPIKey(id)
			if !ok {
				writeNotFound(w)
				return
			}
			if !identity.CanManageTenant(key.TenantID) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			err = a.deps.Repo.DeleteAPIKey(ctx, id)
		}
		if err != nil {
			writeRepoError(w, err)
			return
		}
		audit.Record(ctx, resource+".delete", resource+"/"+id, map[string]any{"id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---------- 密钥/用户特有能力 ----------

func (a *api) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	key, found := a.deps.Repo.GetAPIKey(id)
	if !found {
		writeNotFound(w)
		return
	}
	if !identity.CanManageTenant(key.TenantID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	updated, secret, err := a.deps.Repo.RotateAPIKey(r.Context(), id)
	if err != nil {
		writeRepoError(w, err)
		return
	}
	audit.Record(r.Context(), "apikey.rotate", "apikey/"+id, map[string]any{
		"key_id": id, "key_prefix": updated.KeyPrefix,
		"tenant_id": updated.TenantID, "project_id": updated.ProjectID,
	})
	updated.KeyHash = ""
	writeJSON(w, http.StatusOK, map[string]any{"key": updated, "secret": secret})
}

func (a *api) handleKeyQuota(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	key, found := a.deps.Repo.GetAPIKey(id)
	if !found {
		writeNotFound(w)
		return
	}
	if !identity.CanReadTenant(key.TenantID) {
		writeStatusCode(w, http.StatusForbidden, "forbidden")
		return
	}
	payload := map[string]any{"key_id": id, "limits": map[string]any{
		"rpm_limit":          key.RPMLimit,
		"tpm_limit":          key.TPMLimit,
		"daily_cost_limit":   key.DailyCostLimit,
		"monthly_cost_limit": key.MonthlyCostLimit,
	}}
	if a.deps.Limiter != nil {
		payload["usage"] = a.deps.Limiter.Snapshot(id, key.ProjectID, key.TenantID)
	}
	// 三层是独立预算：把 project / tenant 的限额与用量一起给出，
	// 否则"key 没超但租户超了"这种拒绝在控制台上完全看不出来。
	scopes := []map[string]any{}
	if project, ok := a.deps.Repo.GetProject(key.ProjectID); ok {
		scopes = append(scopes, map[string]any{
			"scope": "project:" + project.ID, "label": "project " + project.ID,
			"rpm_limit": project.RPMLimit, "tpm_limit": project.TPMLimit,
			"daily_cost_limit": project.DailyCostLimit, "monthly_cost_limit": project.MonthlyCostLimit,
		})
	}
	if tenant, ok := a.deps.Repo.GetTenant(key.TenantID); ok {
		scopes = append(scopes, map[string]any{
			"scope": "tenant:" + tenant.ID, "label": "tenant " + tenant.ID,
			"rpm_limit": tenant.RPMLimit, "tpm_limit": tenant.TPMLimit,
			"daily_cost_limit": tenant.DailyCostLimit, "monthly_cost_limit": tenant.MonthlyCostLimit,
		})
	}
	if len(scopes) > 0 {
		payload["scope_limits"] = scopes
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *api) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	user, found := a.deps.Repo.LookupUser(id)
	if !found {
		writeNotFound(w)
		return
	}
	// 本人可改自己的密码;管理员可改管辖范围内用户的密码。
	self := identity.Kind == tenancy.IdentityUser && identity.UserID == id
	if !self && !identity.CanManageTenant(user.TenantID) {
		writeStatusCode(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := a.deps.Repo.SetPassword(r.Context(), id, body.Password); err != nil {
		writeRepoError(w, err)
		return
	}
	// 改密后撤销该用户所有会话(含当前会话)。
	_ = a.deps.Repo.DeleteUserSessions(r.Context(), id)
	audit.Record(r.Context(), "user.password", "user/"+id, map[string]any{
		"user_id": id, "self": self, "sessions_revoked": true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *api) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	user, found := a.deps.Repo.LookupUser(id)
	if !found {
		writeNotFound(w)
		return
	}
	if !identity.CanManageTenant(user.TenantID) && identity.UserID != id {
		writeStatusCode(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := a.deps.Repo.DeleteUserSessions(r.Context(), id); err != nil {
		writeRepoError(w, err)
		return
	}
	audit.Record(r.Context(), "user.sessions.revoke", "user/"+id, map[string]any{"user_id": id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 用量 ----------

// usageBucket 一行用量汇总。
type usageBucket struct {
	Group            string  `json:"group"`
	Requests         float64 `json:"requests"`
	PromptTokens     float64 `json:"prompt_tokens"`
	CompletionTokens float64 `json:"completion_tokens"`
	TotalTokens      float64 `json:"total_tokens"`
	// CachedTokens 命中 prompt cache 的输入量：控制台据此显示命中率。
	// 它是 prompt_tokens 的子集（不是额外量），别把它再加进总量。
	CachedTokens float64 `json:"cached_tokens"`
	Cost         float64 `json:"cost"`
}

// handleUsage 按维度汇总 spend 记录。前端"用量"页面直接用这个。
//
//	?group_by=tenant|project|key|model|provider   (默认 tenant)
//	&tenant=&project=&key=                       过滤
//	&from=RFC3339&to=RFC3339                     默认最近 7 天
//
// 数据来自 raw spend + spend_daily 的合并：raw 超过 raw_retention_days 会被 rollup
// 裁剪，只看 raw 会让 7 天以前的历史直接消失（这正是"rollup 表存在但用量路径没用"
// 的问题）。分界点用 rollup 的 watermark，两侧不重叠，既不重复也不漏算。
func (a *api) handleUsage(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	if a.deps.TS == nil {
		writeError(w, http.StatusServiceUnavailable, "usage storage unavailable")
		return
	}
	q := r.URL.Query()
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "tenant"
	}
	switch groupBy {
	case "tenant", "project", "key", "model", "provider":
	default:
		writeError(w, http.StatusBadRequest, "group_by must be one of tenant|project|key|model|provider")
		return
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -7)
	to := now
	if v := q.Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be RFC3339")
			return
		}
		from = parsed
	}
	if v := q.Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to must be RFC3339")
			return
		}
		to = parsed
	}
	if from.After(to) {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if maxDays := a.deps.UsageMaxDays; maxDays > 0 && to.Sub(from) > time.Duration(maxDays)*24*time.Hour {
		from = to.AddDate(0, 0, -maxDays)
	}

	// 过滤条件：先按身份收窄，再允许管理员显式指定。
	filters := map[string]string{}
	if scope := identity.TenantScope(); scope != "" {
		filters["tenant"] = scope
	}
	if v := q.Get("tenant"); v != "" {
		if !identity.CanReadTenant(v) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		filters["tenant"] = v
	}
	if v := q.Get("project"); v != "" {
		filters["project"] = v
	}
	if v := q.Get("key"); v != "" {
		filters["key"] = v
	}

	dims := []string{}
	if a.deps.RollupDims != nil {
		dims = a.deps.RollupDims()
	}
	var cutoff time.Time
	if a.deps.SpendCutoff != nil {
		cutoff = a.deps.SpendCutoff(r.Context())
	}
	split := spendagg.Plan(from, to, spendagg.EffectiveCutoff(cutoff))

	// raw 的 tag 一定齐全，可以下推；daily 行数很少（天 × 维度），
	// 且历史行可能不带某些 tag，因此不做下推、在内存里过滤并显式报告缺口。
	items, err := spendagg.Collect(r.Context(), a.deps.TS, split, filters, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query usage failed")
		return
	}

	buckets := map[string]*usageBucket{}
	order := make([]string, 0, 16)
	rawRows := 0
	dailyRows := 0
	dailyHasDim := false
	dailyUnattributed := 0
	for _, item := range items {
		if !matchTags(item.Tags, filters) {
			if item.Daily {
				dailyUnattributed++
			}
			continue
		}
		if item.Daily {
			dailyRows++
			if item.Tags[groupBy] != "" {
				dailyHasDim = true
			}
			// 过滤器里但历史行没有的 tag：这一行无法归属，记下来用于降级提示。
			for key := range filters {
				if item.Tags[key] == "" {
					dailyUnattributed++
					break
				}
			}
		} else {
			rawRows++
		}
		group := item.Tags[groupBy]
		if group == "" {
			group = "(none)"
		}
		b, ok := buckets[group]
		if !ok {
			b = &usageBucket{Group: group}
			buckets[group] = b
			order = append(order, group)
		}
		b.Requests += item.Requests
		b.PromptTokens += spendagg.Number(item.Fields["prompt_tokens"])
		b.CompletionTokens += spendagg.Number(item.Fields["completion_tokens"])
		b.TotalTokens += spendagg.Number(item.Fields["total_tokens"])
		b.CachedTokens += spendagg.Number(item.Fields["cached_tokens"])
		b.Cost += spendagg.Number(item.Fields["cost"])
	}

	rows := make([]*usageBucket, 0, len(order))
	for _, group := range order {
		rows = append(rows, buckets[group])
	}
	// 成本倒序；同额按 group 名排序，保证结果稳定（便于前端/测试比对）。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Cost == rows[j].Cost {
			return rows[i].Group < rows[j].Group
		}
		return rows[i].Cost > rows[j].Cost
	})

	payload := map[string]any{
		"group_by": groupBy,
		"from":     from,
		"to":       to,
		"source":   usageSource(rawRows, dailyRows),
		"items":    rows,
	}
	if dailyRows > 0 {
		payload["covered_from"] = split.Cutoff
	}
	if len(dims) > 0 {
		payload["daily_dimensions"] = dims
	}
	// 降级说明：历史汇总缺少该维度（或缺少过滤维度）时，历史部分无法精确归属。
	if dailyRows > 0 && !dailyHasDim {
		payload["truncated"] = true
		if spendagg.HasDim(dims, groupBy) {
			payload["degraded_reason"] = "historical rollup rows were aggregated before \"" + groupBy +
				"\" became a rollup dimension; history before " + split.Cutoff.Format(time.RFC3339) + " cannot be attributed to it"
		} else {
			payload["degraded_reason"] = "dimension \"" + groupBy +
				"\" is not part of spend.rollup_dimensions; history before " + split.Cutoff.Format(time.RFC3339) + " cannot be attributed to it"
		}
	} else if dailyUnattributed > 0 {
		payload["truncated"] = true
		payload["degraded_reason"] = "historical rollup rows lack one of the filter dimensions; " +
			"part of the history cannot be attributed"
	}

	writeJSON(w, http.StatusOK, payload)
}

// usageSource 与 /api/spend/token 保持同一套语义：按**实际命中的记录**
// 判断来源，而不是按规划出来的区间，避免"区间里有 daily 但一条都没命中"时
// 前端显示成 mixed。
func usageSource(rawRows, dailyRows int) string {
	switch {
	case rawRows > 0 && dailyRows > 0:
		return "mixed"
	case rawRows > 0:
		return "raw"
	case dailyRows > 0:
		return "daily"
	default:
		return "empty"
	}
}

// matchTags 判断一条记录的 tag 是否满足全部过滤条件（内存侧兜底）。
func matchTags(tags map[string]string, filters map[string]string) bool {
	for key, want := range filters {
		if tags[key] != want {
			return false
		}
	}
	return true
}

// ---------- 审计 / 告警 ----------

// handleAudit 查询审计日志。全局管理员看全部；租户角色只能看本租户相关记录。
func (a *api) handleAudit(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	if a.deps.Audit == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "limit": 0, "offset": 0})
		return
	}
	page, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := audit.Query{
		Actor: r.URL.Query().Get("actor"), Action: r.URL.Query().Get("action"),
		Resource: r.URL.Query().Get("resource"), Limit: page.limit, Offset: page.offset,
	}
	if v := r.URL.Query().Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be RFC3339")
			return
		}
		q.From = parsed
	}
	if v := r.URL.Query().Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to must be RFC3339")
			return
		}
		q.To = parsed
	}
	// 租户范围：受限身份强制只看本租户，管理员可用 ?tenant= 过滤。
	if scope := identity.TenantScope(); scope != "" {
		q.TenantID = scope
	} else if v := r.URL.Query().Get("tenant"); v != "" {
		if !identity.CanReadTenant(v) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		q.TenantID = v
	}
	result, err := a.deps.Audit.Query(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query audit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  result.Items,
		"total":  result.Total,
		"limit":  result.Limit,
		"offset": result.Offset,
	})
}

// handleAlerts 查询告警。全局管理员看全部；租户角色只看本租户。
func (a *api) handleAlerts(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	if a.deps.Alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "limit": 0, "offset": 0})
		return
	}
	page, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := alerts.Query{
		State: r.URL.Query().Get("state"), Rule: r.URL.Query().Get("rule"),
		Limit: page.limit, Offset: page.offset,
	}
	if scope := identity.TenantScope(); scope != "" {
		q.TenantID = scope
	} else if v := r.URL.Query().Get("tenant"); v != "" {
		if !identity.CanReadTenant(v) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		q.TenantID = v
	}
	items, total, err := a.deps.Alerts.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query alerts failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  page.limit,
		"offset": page.offset,
	})
}

// withLimits 把各层限额并进审计 detail（0 也要写出来，便于看出"从有到无"）。
func withLimits(detail map[string]any, rpm, tpm int, daily, monthly float64) map[string]any {
	detail["rpm_limit"] = rpm
	detail["tpm_limit"] = tpm
	detail["daily_cost_limit"] = daily
	detail["monthly_cost_limit"] = monthly
	return detail
}

// ---------- 工具 ----------

func (a *api) requireIdentity(w http.ResponseWriter, r *http.Request) (*tenancy.Identity, bool) {
	identity, ok := tenancy.FromContext(r.Context())
	if !ok || identity == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	return identity, true
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return false
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is required")
		return false
	}
	if err := json.Unmarshal(body, target); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return false
	}
	return true
}

func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeRepoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeRepoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenancy.ErrNotFound):
		writeNotFound(w)
	case errors.Is(err, tenancy.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, tenancy.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not found")
}

func writeStatusCode(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func applyString(patch map[string]any, key string, target *string) {
	if v, ok := patch[key]; ok {
		if s, ok := v.(string); ok {
			*target = s
		}
	}
}

func applyStringSlice(patch map[string]any, key string, target *[]string) {
	v, ok := patch[key]
	if !ok {
		return
	}
	// 显式 null 或 [] 表示清空
	if v == nil {
		*target = nil
		return
	}
	raw, ok := v.([]any)
	if !ok {
		return
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	*target = out
}

func applyInt(patch map[string]any, key string, target *int) {
	if v, ok := patch[key]; ok {
		switch n := v.(type) {
		case float64:
			*target = int(n)
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				*target = parsed
			}
		}
	}
}

func applyFloat(patch map[string]any, key string, target *float64) {
	if v, ok := patch[key]; ok {
		switch n := v.(type) {
		case float64:
			*target = n
		case string:
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				*target = parsed
			}
		}
	}
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return parsed
		}
	}
	return 0
}
