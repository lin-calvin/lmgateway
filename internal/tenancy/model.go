// Package tenancy implements multi-tenancy for lmgateway:
//
//	tenant → project → API key   (数据面凭据)
//	user 账号 + 会话登录           (管理面凭据)
//
// 设计约束:新增包,对既有代码最小侵入。所有数据走既有 store.JSONStore
// (键前缀 tenant/ project/ user/ apikey/ session/),不进入 config.Config,
// 因此不参与路由编译,也不需要 YAML 基线。
package tenancy

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 资源状态
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// 用户角色
const (
	RoleAdmin       = "admin"        // 全局管理员:可管理所有租户
	RoleTenantAdmin = "tenant_admin" // 租户管理员:管理本租户下 project/user/key
	RoleMember      = "member"       // 租户成员:只读本租户
)

// 存储键前缀
const (
	PrefixTenant  = "tenant/"
	PrefixProject = "project/"
	PrefixUser    = "user/"
	PrefixAPIKey  = "apikey/"
	PrefixSession = "session/"
)

// 凭据前缀约定:会话 token 与 API key 可通过前缀快速区分,无需先查库。
const (
	SessionTokenPrefix = "sess_"
	APIKeyPrefix       = "sk-lm-"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidID 校验 tenant/project 的 id(slug)。刻意不允许下划线:JSONStore 的
// List 在 sqlite/pg 上用 LIKE 前缀匹配且不转义 %,_,含下划线会过度匹配。
func ValidID(id string) bool { return slugRe.MatchString(id) }

// Tenant 最外层隔离单元。
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Project 租户内的隔离单元,API key 挂在 project 上。
type Project struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User 管理面账号。RoleAdmin 的 TenantID 为空(全局)。
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Name         string     `json:"name,omitempty"`
	PasswordHash string     `json:"password_hash"`
	Role         string     `json:"role"`
	TenantID     string     `json:"tenant_id,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// APIKey 数据面凭据。仓库里只存 hash,明文仅在创建/轮换时返回一次。
type APIKey struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"tenant_id"`
	ProjectID        string     `json:"project_id"`
	Name             string     `json:"name,omitempty"`
	KeyHash          string     `json:"key_hash,omitempty"` // 响应前一律清空,不外泄
	KeyPrefix        string     `json:"key_prefix"`
	Status           string     `json:"status"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	AllowedModels    []string   `json:"allowed_models,omitempty"` // 空 = 允许全部
	DeniedModels     []string   `json:"denied_models,omitempty"`  // 优先级高于 allow
	RPMLimit         int        `json:"rpm_limit,omitempty"`      // 0 = 不限
	TPMLimit         int        `json:"tpm_limit,omitempty"`      // 0 = 不限
	DailyCostLimit   float64    `json:"daily_cost_limit,omitempty"`
	MonthlyCostLimit float64    `json:"monthly_cost_limit,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	CreatedBy        string     `json:"created_by,omitempty"`
}

// Active 判断凭据当前是否可用。
func (k *APIKey) Active(now time.Time) bool {
	if k.Status != StatusActive {
		return false
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return false
	}
	return true
}

// AllowsModel 白名单/黑名单判定。model 是客户端请求的原始模型名(别名改写之前)。
//
// 规则:deny 优先;allow 为空表示"不限";支持末尾 * 的通配前缀(如 "gpt-*")。
func (k *APIKey) AllowsModel(model string) bool {
	for _, d := range k.DeniedModels {
		if matchModelPattern(d, model) {
			return false
		}
	}
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, a := range k.AllowedModels {
		if matchModelPattern(a, model) {
			return true
		}
	}
	return false
}

func matchModelPattern(pattern, model string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == model
}

// Session 登录会话。键是 token 的 sha256,值里不含明文。
type Session struct {
	UserID     string    `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Expired 判断会话是否过期。
func (s *Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// ValidateTenant 校验租户字段。
func ValidateTenant(t *Tenant) error {
	if !ValidID(t.ID) {
		return fmt.Errorf("tenant id must match %s", slugRe.String())
	}
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("tenant name is required")
	}
	return validateStatus(t.Status)
}

// ValidateProject 校验项目字段。
func ValidateProject(p *Project) error {
	if !ValidID(p.ID) {
		return fmt.Errorf("project id must match %s", slugRe.String())
	}
	if !ValidID(p.TenantID) {
		return fmt.Errorf("project tenant_id must match %s", slugRe.String())
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("project name is required")
	}
	return validateStatus(p.Status)
}

// ValidateUser 校验用户字段。
func ValidateUser(u *User) error {
	if strings.TrimSpace(u.Email) == "" || !strings.Contains(u.Email, "@") {
		return fmt.Errorf("user email is invalid")
	}
	switch u.Role {
	case RoleAdmin, RoleTenantAdmin, RoleMember:
	default:
		return fmt.Errorf("user role must be one of %s/%s/%s", RoleAdmin, RoleTenantAdmin, RoleMember)
	}
	if u.Role != RoleAdmin && !ValidID(u.TenantID) {
		return fmt.Errorf("non-admin user requires a valid tenant_id")
	}
	if u.Role == RoleAdmin && u.TenantID != "" {
		return fmt.Errorf("admin user must not have a tenant_id")
	}
	return validateStatus(u.Status)
}

// ValidateAPIKey 校验 API key 字段。
func ValidateAPIKey(k *APIKey) error {
	if !ValidID(k.TenantID) {
		return fmt.Errorf("apikey tenant_id must match %s", slugRe.String())
	}
	if !ValidID(k.ProjectID) {
		return fmt.Errorf("apikey project_id must match %s", slugRe.String())
	}
	if k.RPMLimit < 0 || k.TPMLimit < 0 {
		return fmt.Errorf("rpm/tpm limits must not be negative")
	}
	if k.DailyCostLimit < 0 || k.MonthlyCostLimit < 0 {
		return fmt.Errorf("cost limits must not be negative")
	}
	return validateStatus(k.Status)
}

func validateStatus(status string) error {
	switch status {
	case StatusActive, StatusDisabled:
		return nil
	default:
		return fmt.Errorf("status must be %s or %s", StatusActive, StatusDisabled)
	}
}
