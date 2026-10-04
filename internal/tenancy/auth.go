package tenancy

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"
)

// 鉴权/授权错误,API 层据此映射状态码。
var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("forbidden")
)

// IdentityKind 凭据类型。
type IdentityKind string

const (
	IdentityMaster IdentityKind = "master" // 运维 master key
	IdentityUser   IdentityKind = "user"   // 登录用户(管理面)
	IdentityAPIKey IdentityKind = "apikey" // 租户 API key(数据面)
)

// Identity 是一次请求的身份快照。数据面配额与记账都基于它。
type Identity struct {
	Kind IdentityKind

	// 管理面字段
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role,omitempty"`

	// 归属字段(数据面/租户范围)
	TenantID  string `json:"tenant_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`

	// 配额快照(仅 API key 有)
	AllowedModels    []string `json:"allowed_models,omitempty"`
	DeniedModels     []string `json:"denied_models,omitempty"`
	RPMLimit         int      `json:"rpm_limit,omitempty"`
	TPMLimit         int      `json:"tpm_limit,omitempty"`
	DailyCostLimit   float64  `json:"daily_cost_limit,omitempty"`
	MonthlyCostLimit float64  `json:"monthly_cost_limit,omitempty"`
}

// IsAdmin 全局管理员(master key 或 role=admin)。
func (i *Identity) IsAdmin() bool {
	if i == nil {
		return false
	}
	return i.Kind == IdentityMaster || i.Role == RoleAdmin
}

// CanManageTenant 判断能否写入/管理某租户下的资源。
func (i *Identity) CanManageTenant(tenantID string) bool {
	if i == nil {
		return false
	}
	if i.IsAdmin() {
		return true
	}
	return i.Role == RoleTenantAdmin && i.TenantID != "" && i.TenantID == tenantID
}

// CanReadTenant 判断能否读取某租户的数据。
func (i *Identity) CanReadTenant(tenantID string) bool {
	if i == nil {
		return false
	}
	if i.IsAdmin() {
		return true
	}
	return i.TenantID != "" && i.TenantID == tenantID
}

// TenantScope 返回该身份可见的租户范围:空字符串表示"全部"(管理员)。
func (i *Identity) TenantScope() string {
	if i == nil || i.IsAdmin() {
		return ""
	}
	return i.TenantID
}

// AllowsModel 数据面模型授权。非 API key 凭据不受限。
func (i *Identity) AllowsModel(model string) bool {
	if i == nil || i.Kind != IdentityAPIKey {
		return true
	}
	k := &APIKey{AllowedModels: i.AllowedModels, DeniedModels: i.DeniedModels}
	return k.AllowsModel(model)
}

// Authenticator 把 HTTP 凭据解析成 Identity。
//
// 支持的凭据(按优先级):
//  1. master key(配置里给的运维密钥)→ IdentityMaster,可访问管理面与数据面
//  2. 会话 token(sess_ 前缀)→ IdentityUser,仅管理面
//  3. API key(sk-lm- 前缀)→ IdentityAPIKey,仅数据面
type Authenticator struct {
	masterKey string
	repo      *Repository
}

func NewAuthenticator(masterKey string, repo *Repository) *Authenticator {
	return &Authenticator{masterKey: strings.TrimSpace(masterKey), repo: repo}
}

// MasterKeyConfigured 是否配置了运维密钥。
func (a *Authenticator) MasterKeyConfigured() bool { return a.masterKey != "" }

// Repository 暴露仓储(API 层用)。
func (a *Authenticator) Repository() *Repository { return a.repo }

// Disabled 表示完全未启用鉴权(既没有 master key):保持上游既有的本地开发体验。
func (a *Authenticator) Disabled() bool { return a.masterKey == "" }

// Authenticate 解析请求凭据。无凭据时返回 ErrUnauthenticated。
func (a *Authenticator) Authenticate(r *http.Request) (*Identity, error) {
	token := ExtractToken(r)
	if token == "" {
		return nil, ErrUnauthenticated
	}
	if a.masterKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.masterKey)) == 1 {
		return &Identity{Kind: IdentityMaster, Role: RoleAdmin}, nil
	}
	if a.repo == nil {
		return nil, ErrUnauthenticated
	}
	now := time.Now()
	switch {
	case LooksLikeSession(token):
		session, ok := a.repo.LookupSession(HashSecret(token))
		if !ok || session.Expired(now) {
			return nil, ErrUnauthenticated
		}
		user, ok := a.repo.LookupUser(session.UserID)
		if !ok || user.Status != StatusActive {
			return nil, ErrUnauthenticated
		}
		identity := &Identity{
			Kind:     IdentityUser,
			UserID:   user.ID,
			Email:    user.Email,
			Role:     user.Role,
			TenantID: user.TenantID,
		}
		if user.TenantID != "" {
			if tenant, ok := a.repo.GetTenant(user.TenantID); !ok || tenant.Status != StatusActive {
				return nil, ErrUnauthenticated
			}
		}
		return identity, nil
	case LooksLikeAPIKey(token):
		key, ok := a.repo.LookupAPIKey(HashSecret(token))
		if !ok || !key.Active(now) {
			return nil, ErrUnauthenticated
		}
		if tenant, ok := a.repo.GetTenant(key.TenantID); !ok || tenant.Status != StatusActive {
			return nil, ErrForbidden
		}
		if project, ok := a.repo.GetProject(key.ProjectID); !ok || project.Status != StatusActive {
			return nil, ErrForbidden
		}
		a.repo.TouchAPIKey(context.Background(), key.ID)
		return &Identity{
			Kind:             IdentityAPIKey,
			TenantID:         key.TenantID,
			ProjectID:        key.ProjectID,
			KeyID:            key.ID,
			KeyPrefix:        key.KeyPrefix,
			AllowedModels:    key.AllowedModels,
			DeniedModels:     key.DeniedModels,
			RPMLimit:         key.RPMLimit,
			TPMLimit:         key.TPMLimit,
			DailyCostLimit:   key.DailyCostLimit,
			MonthlyCostLimit: key.MonthlyCostLimit,
		}, nil
	default:
		return nil, ErrUnauthenticated
	}
}

// ExtractToken 按既有约定取凭据:X-API-Key 优先,否则 Authorization: Bearer。
// 与 internal/auth 的既有行为保持一致。
func ExtractToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
		return fields[1]
	}
	return ""
}

// Scope 决定某个挂载点接受哪类凭据。
type Scope int

const (
	// ScopeData 数据面(/v1/*):只接受 master key 与 API key。
	ScopeData Scope = iota
	// ScopeAdmin 管理面(/api/*):只接受 master key 与用户会话。
	ScopeAdmin
)

func (s Scope) allows(kind IdentityKind) bool {
	switch s {
	case ScopeData:
		return kind == IdentityMaster || kind == IdentityAPIKey
	case ScopeAdmin:
		return kind == IdentityMaster || kind == IdentityUser
	default:
		return false
	}
}

// Guard 返回一个 HTTP 中间件:校验凭据、把 Identity 注入 context。
// 未配置 master key 且仓储为空时视为"鉴权关闭",放行并注入一个匿名管理员身份,
// 保持与上游既有行为一致(本地开发无需配 key)。
func (a *Authenticator) Guard(scope Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康检查始终公开(与上游既有行为一致;Prometheus 与容器编排都依赖它)。
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if a.openMode() {
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), &Identity{Kind: IdentityMaster, Role: RoleAdmin})))
			return
		}
		identity, err := a.Authenticate(r)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, ErrForbidden) {
				status = http.StatusForbidden
			}
			WriteAuthError(w, status, err.Error())
			return
		}
		if !scope.allows(identity.Kind) {
			// 凭据有效但用错了面:API key 不能调管理 API,会话不能调数据面。
			WriteAuthError(w, http.StatusForbidden, "credential not permitted for this endpoint")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

// GuardConfig 网关配置管理面(/api/*)的中间件。
//
// 只接受两类凭据:
//   - master key(运维身份)
//   - **全局管理员**的会话(role=admin)
//
// 租户管理员/成员的会话即使合法也一律 403 —— 网关配置(provider/model/rule/pool)是
// 所有租户共享的,不属于任何单个租户;数据面 API key 同样不能碰这里。
// 这就是"角色判断下沉到接口层":前端隐藏只是体验,真正的边界在这里。
func (a *Authenticator) GuardConfig(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.openMode() {
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), &Identity{Kind: IdentityMaster, Role: RoleAdmin})))
			return
		}
		identity, err := a.Authenticate(r)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, ErrForbidden) {
				status = http.StatusForbidden
			}
			WriteAuthError(w, status, err.Error())
			return
		}
		if identity.Kind == IdentityAPIKey || (identity.Kind == IdentityUser && identity.Role != RoleAdmin) {
			WriteAuthError(w, http.StatusForbidden, "gateway configuration requires a global administrator")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

func (a *Authenticator) openMode() bool {
	return a.masterKey == "" && (a.repo == nil || !a.repo.HasAnyUser())
}

// GuardSoft 是管理 API 用的宽松中间件:凭据有效则注入身份,无凭据/凭据失效
// 也放行(由各 handler 自己用 requireIdentity 决定是否需要登录)。
// 这样登录与首次初始化(此时还不存在任何会话)才能被访问。
func (a *Authenticator) GuardSoft(scope Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := a.Authenticate(r)
		if err != nil || identity == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !scope.allows(identity.Kind) {
			// 凭据有效但用错了面(例如拿数据面 key 调管理 API)。
			WriteAuthError(w, http.StatusForbidden, "credential not permitted for this endpoint")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

// WriteAuthError 输出与既有 internal/auth 一致的错误形状。
func WriteAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lmgateway"`)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + quoteJSON(message) + `}`))
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

type identityCtxKey struct{}

// WithIdentity 把身份写进 context。
func WithIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, identity)
}

// FromContext 读取身份。
func FromContext(ctx context.Context) (*Identity, bool) {
	identity, ok := ctx.Value(identityCtxKey{}).(*Identity)
	return identity, ok && identity != nil
}
