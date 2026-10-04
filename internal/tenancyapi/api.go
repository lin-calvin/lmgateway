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
package tenancyapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	a.issueSession(w, r, user)
}

func (a *api) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	user, ok := a.deps.Repo.LookupUserByEmail(req.Email)
	if !ok || user.Status != tenancy.StatusActive || !tenancy.VerifyPassword(user.PasswordHash, req.Password) {
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

func (a *api) list(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.requireIdentity(w, r)
		if !ok {
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
			items := a.visibleTenants(identity)
			writeJSON(w, http.StatusOK, map[string]any{"items": items})
		case "projects":
			writeJSON(w, http.StatusOK, map[string]any{"items": a.deps.Repo.ListProjects(tenantFilter)})
		case "users":
			writeJSON(w, http.StatusOK, map[string]any{"items": a.deps.Repo.ListUsers(tenantFilter)})
		case "keys":
			writeJSON(w, http.StatusOK, map[string]any{"items": a.deps.Repo.ListAPIKeys(tenantFilter, projectFilter)})
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
			})
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
			})
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 用量 ----------

// handleUsage 按维度汇总 spend 记录。前端"用量"页面直接用这个。
//
//	?group_by=tenant|project|key|model   (默认 tenant)
//	&tenant=&project=&key=               过滤
//	&from=RFC3339&to=RFC3339            默认最近 7 天
func (a *api) handleUsage(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r)
	if !ok {
		return
	}
	if a.deps.TS == nil {
		writeError(w, http.StatusServiceUnavailable, "usage storage unavailable")
		return
	}
	groupBy := r.URL.Query().Get("group_by")
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
	if v := r.URL.Query().Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be RFC3339")
			return
		}
		from = parsed
	}
	if v := r.URL.Query().Get("to"); v != "" {
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

	records, err := a.deps.TS.Query(r.Context(), store.TSQuery{Stream: "spend", From: from, To: to})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query usage failed")
		return
	}

	scope := identity.TenantScope()
	type bucket struct {
		Group            string  `json:"group"`
		Requests         int     `json:"requests"`
		PromptTokens     float64 `json:"prompt_tokens"`
		CompletionTokens float64 `json:"completion_tokens"`
		TotalTokens      float64 `json:"total_tokens"`
		Cost             float64 `json:"cost"`
	}
	buckets := map[string]*bucket{}
	for _, rec := range records {
		if scope != "" && rec.Tags["tenant"] != scope {
			continue
		}
		if v := r.URL.Query().Get("tenant"); v != "" && rec.Tags["tenant"] != v {
			continue
		}
		if v := r.URL.Query().Get("project"); v != "" && rec.Tags["project"] != v {
			continue
		}
		if v := r.URL.Query().Get("key"); v != "" && rec.Tags["key"] != v {
			continue
		}
		group := rec.Tags[groupBy]
		if group == "" {
			group = "(none)"
		}
		b, ok := buckets[group]
		if !ok {
			b = &bucket{Group: group}
			buckets[group] = b
		}
		b.Requests++
		b.PromptTokens += number(rec.Fields["prompt_tokens"])
		b.CompletionTokens += number(rec.Fields["completion_tokens"])
		b.TotalTokens += number(rec.Fields["total_tokens"])
		b.Cost += number(rec.Fields["cost"])
	}
	items := make([]*bucket, 0, len(buckets))
	for _, b := range buckets {
		items = append(items, b)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Cost > items[j].Cost })

	writeJSON(w, http.StatusOK, map[string]any{
		"group_by": groupBy,
		"from":     from,
		"to":       to,
		"items":    items,
	})
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
