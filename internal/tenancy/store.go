package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"lmgateway/internal/store"
)

// 常见错误,供 API 层映射 HTTP 状态码。
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Repository 是租户数据的唯一入口。数据落在 JSONStore(键前缀见 model.go),
// 同时在内存里维护索引:数据面鉴权是每请求热路径,不能每次扫库。
//
// 一致性策略:写操作直接写 store,再刷新内存;store 的 Watch 保证多实例/多进程
// 场景下也能在秒级收敛;另有周期性兜底刷新。
type Repository struct {
	js store.JSONStore

	mu          sync.RWMutex
	tenants     map[string]*Tenant
	projects    map[string]*Project
	users       map[string]*User
	userByEmail map[string]*User
	keys        map[string]*APIKey
	keyByHash   map[string]*APIKey
	sessions    map[string]*Session

	refreshMu sync.Mutex
	onChange  []func()
}

// NewRepository 装载全部租户数据并订阅变更。
func NewRepository(ctx context.Context, js store.JSONStore) (*Repository, error) {
	r := &Repository{js: js}
	if err := r.Refresh(ctx); err != nil {
		return nil, err
	}
	if js != nil {
		_, err := js.Watch(ctx, "", func(store.JSONChange) {
			// 配置写入也会触发这里;刷新是幂等的,直接做。
			_ = r.Refresh(ctx)
		})
		if err != nil {
			log.Printf("[tenancy] watch unavailable (fallback to periodic refresh): %v", err)
		}
		go r.periodicRefresh(ctx)
	}
	return r, nil
}

// OnChange 注册变更回调(配额计数器用它做校准)。
func (r *Repository) OnChange(fn func()) {
	r.mu.Lock()
	r.onChange = append(r.onChange, fn)
	r.mu.Unlock()
}

func (r *Repository) periodicRefresh(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Refresh(ctx); err != nil {
				log.Printf("[tenancy] periodic refresh failed: %v", err)
			}
		}
	}
}

// Refresh 从 store 重新装载全部索引。
//
// 必须串行化：如果有两个 Refresh 交叠（写后刷新 + Watch 回调 + 定期兜底），
// 先启动但后完成的那个会把**旧快照**盖到新快照上，导致"写库成功、索引里却没有"
// —— 表现为刚创建的密钥立刻 401。用 refreshMu 保证同一时刻只有一个装载在进行，
// 于是每次装载都在上一次换入之后开始读，快照单调不回退。
func (r *Repository) Refresh(ctx context.Context) error {
	if r.js == nil {
		r.mu.Lock()
		if r.tenants == nil {
			r.initMapsLocked()
		}
		r.mu.Unlock()
		return nil
	}

	r.refreshMu.Lock()
	tenants, err := r.loadTenants(ctx)
	if err != nil {
		r.refreshMu.Unlock()
		return err
	}
	projects, err := r.loadProjects(ctx)
	if err != nil {
		r.refreshMu.Unlock()
		return err
	}
	users, err := r.loadUsers(ctx)
	if err != nil {
		r.refreshMu.Unlock()
		return err
	}
	keys, err := r.loadKeys(ctx)
	if err != nil {
		r.refreshMu.Unlock()
		return err
	}
	sessions, err := r.loadSessions(ctx)
	if err != nil {
		r.refreshMu.Unlock()
		return err
	}

	r.mu.Lock()
	r.tenants = tenants
	r.projects = projects
	r.users = users
	r.userByEmail = make(map[string]*User, len(users))
	for _, u := range users {
		r.userByEmail[strings.ToLower(u.Email)] = u
	}
	r.keys = keys
	r.keyByHash = make(map[string]*APIKey, len(keys))
	for _, k := range keys {
		r.keyByHash[k.KeyHash] = k
	}
	r.sessions = sessions
	callbacks := append([]func(){}, r.onChange...)
	r.mu.Unlock()
	r.refreshMu.Unlock()

	// 回调放在锁外，避免回调里再触发写入造成自锁
	for _, fn := range callbacks {
		fn()
	}
	return nil
}

func (r *Repository) initMapsLocked() {
	r.tenants = map[string]*Tenant{}
	r.projects = map[string]*Project{}
	r.users = map[string]*User{}
	r.userByEmail = map[string]*User{}
	r.keys = map[string]*APIKey{}
	r.keyByHash = map[string]*APIKey{}
	r.sessions = map[string]*Session{}
}

func (r *Repository) loadTenants(ctx context.Context) (map[string]*Tenant, error) {
	docs, err := r.js.List(ctx, PrefixTenant)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Tenant, len(docs))
	for _, doc := range docs {
		var t Tenant
		if err := json.Unmarshal(doc.Data, &t); err != nil {
			log.Printf("[tenancy] skip malformed %s: %v", doc.Key, err)
			continue
		}
		if t.ID == "" {
			t.ID = strings.TrimPrefix(doc.Key, PrefixTenant)
		}
		out[t.ID] = &t
	}
	return out, nil
}

func (r *Repository) loadProjects(ctx context.Context) (map[string]*Project, error) {
	docs, err := r.js.List(ctx, PrefixProject)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Project, len(docs))
	for _, doc := range docs {
		var p Project
		if err := json.Unmarshal(doc.Data, &p); err != nil {
			log.Printf("[tenancy] skip malformed %s: %v", doc.Key, err)
			continue
		}
		if p.ID == "" {
			p.ID = strings.TrimPrefix(doc.Key, PrefixProject)
		}
		out[p.ID] = &p
	}
	return out, nil
}

func (r *Repository) loadUsers(ctx context.Context) (map[string]*User, error) {
	docs, err := r.js.List(ctx, PrefixUser)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*User, len(docs))
	for _, doc := range docs {
		var u User
		if err := json.Unmarshal(doc.Data, &u); err != nil {
			log.Printf("[tenancy] skip malformed %s: %v", doc.Key, err)
			continue
		}
		if u.ID == "" {
			u.ID = strings.TrimPrefix(doc.Key, PrefixUser)
		}
		out[u.ID] = &u
	}
	return out, nil
}

func (r *Repository) loadKeys(ctx context.Context) (map[string]*APIKey, error) {
	docs, err := r.js.List(ctx, PrefixAPIKey)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*APIKey, len(docs))
	for _, doc := range docs {
		var k APIKey
		if err := json.Unmarshal(doc.Data, &k); err != nil {
			log.Printf("[tenancy] skip malformed %s: %v", doc.Key, err)
			continue
		}
		if k.ID == "" {
			k.ID = strings.TrimPrefix(doc.Key, PrefixAPIKey)
		}
		out[k.ID] = &k
	}
	return out, nil
}

func (r *Repository) loadSessions(ctx context.Context) (map[string]*Session, error) {
	docs, err := r.js.List(ctx, PrefixSession)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Session, len(docs))
	now := time.Now()
	for _, doc := range docs {
		var s Session
		if err := json.Unmarshal(doc.Data, &s); err != nil {
			continue
		}
		if s.Expired(now) {
			continue // 过期会话不载入内存(懒清理)
		}
		out[strings.TrimPrefix(doc.Key, PrefixSession)] = &s
	}
	return out, nil
}

// ---------- 查询(数据面热路径) ----------

// LookupAPIKey 按 key 的哈希查凭据。返回副本,调用方不可修改内部状态。
//
// 索引未命中时会**回源查一次**并把结果补进索引：鉴权是安全路径，
// 宁可慢一点，也不能因为索引短暂滞后就把合法密钥判成无效（表现为刚创建的密钥立刻 401）。
func (r *Repository) LookupAPIKey(hash string) (*APIKey, bool) {
	r.mu.RLock()
	k, ok := r.keyByHash[hash]
	r.mu.RUnlock()
	if ok {
		return cloneKey(k), true
	}
	if r.js == nil {
		return nil, false
	}
	found, ok := r.findKeyByHashInStore(context.Background(), hash)
	if !ok {
		return nil, false
	}
	r.healKey(found)
	return cloneKey(found), true
}

func cloneKey(k *APIKey) *APIKey {
	clone := *k
	clone.AllowedModels = append([]string(nil), k.AllowedModels...)
	clone.DeniedModels = append([]string(nil), k.DeniedModels...)
	return &clone
}

// findKeyByHashInStore 直接扫 store 找哈希匹配的密钥（仅索引未命中时调用）。
func (r *Repository) findKeyByHashInStore(ctx context.Context, hash string) (*APIKey, bool) {
	keys, err := r.loadKeys(ctx)
	if err != nil {
		log.Printf("[tenancy] apikey lookup fallback failed: %v", err)
		return nil, false
	}
	for _, k := range keys {
		if k.KeyHash == hash {
			return k, true
		}
	}
	return nil, false
}

// healKey 把回源查到的密钥补进索引，避免后续请求都回源。
func (r *Repository) healKey(k *APIKey) {
	r.mu.Lock()
	if r.keys == nil || r.keyByHash == nil {
		r.initMapsLocked()
	}
	r.keys[k.ID] = k
	r.keyByHash[k.KeyHash] = k
	r.mu.Unlock()
}

// verifyStoredKeyHash 回源确认刚写入的密钥哈希与将要返回给客户端的明文一致。
//
// 安全路径上的"写后校验"：万一存储环节出问题（并发/异常），宁可返回错误让调用方重试，
// 也不要把一把**用户拿到却用不了**的密钥发出去——那种故障排查起来极其费时。
func (r *Repository) verifyStoredKeyHash(ctx context.Context, id, wantHash string) error {
	if r.js == nil {
		return nil
	}
	doc, err := r.js.Get(ctx, PrefixAPIKey+id)
	if err != nil {
		return fmt.Errorf("verify api key %s: %w", id, err)
	}
	var stored APIKey
	if err := json.Unmarshal(doc.Data, &stored); err != nil {
		return fmt.Errorf("verify api key %s: %w", id, err)
	}
	if stored.KeyHash != wantHash {
		return fmt.Errorf("api key %s failed write verification: stored hash does not match the generated secret", id)
	}
	return nil
}

// LookupSession 按 token 哈希查会话。
func (r *Repository) LookupSession(hash string) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[hash]
	if !ok {
		return nil, false
	}
	clone := *s
	return &clone, true
}

// LookupUser 按 id 查用户。
func (r *Repository) LookupUser(id string) (*User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, false
	}
	clone := *u
	return &clone, true
}

// LookupUserByEmail 按邮箱查用户(登录路径)。
func (r *Repository) LookupUserByEmail(email string) (*User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.userByEmail[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, false
	}
	clone := *u
	return &clone, true
}

// GetTenant / GetProject
func (r *Repository) GetTenant(id string) (*Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tenants[id]
	if !ok {
		return nil, false
	}
	clone := *t
	return &clone, true
}

func (r *Repository) GetProject(id string) (*Project, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.projects[id]
	if !ok {
		return nil, false
	}
	clone := *p
	return &clone, true
}

func (r *Repository) GetAPIKey(id string) (*APIKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.keys[id]
	if !ok {
		return nil, false
	}
	return cloneKey(k), true
}

// ---------- 列表 ----------

func (r *Repository) ListTenants() []*Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		clone := *t
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Repository) ListProjects(tenantID string) []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*Project{}
	for _, p := range r.projects {
		if tenantID != "" && p.TenantID != tenantID {
			continue
		}
		clone := *p
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Repository) ListUsers(tenantID string) []*User {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*User{}
	for _, u := range r.users {
		if tenantID != "" && u.TenantID != tenantID {
			continue
		}
		clone := *u
		clone.PasswordHash = "" // 永不出库
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Repository) ListAPIKeys(tenantID, projectID string) []*APIKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*APIKey{}
	for _, k := range r.keys {
		if tenantID != "" && k.TenantID != tenantID {
			continue
		}
		if projectID != "" && k.ProjectID != projectID {
			continue
		}
		clone := cloneKey(k)
		clone.KeyHash = "" // hash 也不外泄
		out = append(out, clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------- 写入 ----------

func (r *Repository) put(ctx context.Context, key string, value any) error {
	if r.js == nil {
		return fmt.Errorf("tenancy storage unavailable")
	}
	if _, err := r.js.Put(ctx, key, value); err != nil {
		return err
	}
	return r.Refresh(ctx)
}

// CreateTenant 新建租户。
func (r *Repository) CreateTenant(ctx context.Context, t *Tenant) error {
	if _, ok := r.GetTenant(t.ID); ok {
		return fmt.Errorf("%w: tenant %s already exists", ErrConflict, t.ID)
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	if t.Status == "" {
		t.Status = StatusActive
	}
	if err := ValidateTenant(t); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return r.put(ctx, PrefixTenant+t.ID, t)
}

// UpdateTenant 更新租户(按 id,不可改 id)。
func (r *Repository) UpdateTenant(ctx context.Context, id string, apply func(*Tenant)) (*Tenant, error) {
	current, ok := r.GetTenant(id)
	if !ok {
		return nil, fmt.Errorf("%w: tenant %s", ErrNotFound, id)
	}
	apply(current)
	current.ID = id
	current.UpdatedAt = time.Now().UTC()
	if err := ValidateTenant(current); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return current, r.put(ctx, PrefixTenant+id, current)
}

// DeleteTenant 删除租户。有下级资源时拒绝(避免误删产生孤儿数据)。
func (r *Repository) DeleteTenant(ctx context.Context, id string) error {
	if _, ok := r.GetTenant(id); !ok {
		return fmt.Errorf("%w: tenant %s", ErrNotFound, id)
	}
	if len(r.ListProjects(id)) > 0 {
		return fmt.Errorf("%w: tenant still has projects", ErrConflict)
	}
	if len(r.ListUsers(id)) > 0 {
		return fmt.Errorf("%w: tenant still has users", ErrConflict)
	}
	return r.delete(ctx, PrefixTenant+id)
}

// CreateProject 新建项目。
func (r *Repository) CreateProject(ctx context.Context, p *Project) error {
	if _, ok := r.GetProject(p.ID); ok {
		return fmt.Errorf("%w: project %s already exists", ErrConflict, p.ID)
	}
	tenant, ok := r.GetTenant(p.TenantID)
	if !ok {
		return fmt.Errorf("%w: tenant %s", ErrNotFound, p.TenantID)
	}
	if tenant.Status != StatusActive {
		return fmt.Errorf("%w: tenant %s is %s", ErrInvalid, tenant.ID, tenant.Status)
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	if p.Status == "" {
		p.Status = StatusActive
	}
	if err := ValidateProject(p); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return r.put(ctx, PrefixProject+p.ID, p)
}

func (r *Repository) UpdateProject(ctx context.Context, id string, apply func(*Project)) (*Project, error) {
	current, ok := r.GetProject(id)
	if !ok {
		return nil, fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	apply(current)
	current.ID = id
	current.UpdatedAt = time.Now().UTC()
	if err := ValidateProject(current); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return current, r.put(ctx, PrefixProject+id, current)
}

func (r *Repository) DeleteProject(ctx context.Context, id string) error {
	if _, ok := r.GetProject(id); !ok {
		return fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	if len(r.ListAPIKeys("", id)) > 0 {
		return fmt.Errorf("%w: project still has api keys", ErrConflict)
	}
	return r.delete(ctx, PrefixProject+id)
}

// CreateUser 新建用户,password 为明文(仅此处使用,不落库)。
func (r *Repository) CreateUser(ctx context.Context, u *User, password string) error {
	u.Email = strings.ToLower(strings.TrimSpace(u.Email))
	if _, ok := r.LookupUserByEmail(u.Email); ok {
		return fmt.Errorf("%w: user %s already exists", ErrConflict, u.Email)
	}
	if u.ID == "" {
		u.ID = userIDFromEmail(u.Email)
	}
	if u.Role == "" {
		u.Role = RoleMember
	}
	if u.Status == "" {
		u.Status = StatusActive
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	u.PasswordHash = hash
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now
	if err := ValidateUser(u); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if u.TenantID != "" {
		if _, ok := r.GetTenant(u.TenantID); !ok {
			return fmt.Errorf("%w: tenant %s", ErrNotFound, u.TenantID)
		}
	}
	return r.put(ctx, PrefixUser+u.ID, u)
}

func (r *Repository) UpdateUser(ctx context.Context, id string, apply func(*User)) (*User, error) {
	current, ok := r.LookupUser(id)
	if !ok {
		return nil, fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	hash := current.PasswordHash
	apply(current)
	current.ID = id
	current.PasswordHash = hash // 密码只能通过 SetPassword 改
	current.UpdatedAt = time.Now().UTC()
	if err := ValidateUser(current); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return current, r.put(ctx, PrefixUser+id, current)
}

// SetPassword 改密。
func (r *Repository) SetPassword(ctx context.Context, id, password string) error {
	u, ok := r.LookupUser(id)
	if !ok {
		return fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	u.PasswordHash = hash
	u.UpdatedAt = time.Now().UTC()
	return r.put(ctx, PrefixUser+id, u)
}

// MarkLogin 记录登录时间与清理该用户的旧会话。
func (r *Repository) MarkLogin(ctx context.Context, id string) {
	u, ok := r.LookupUser(id)
	if !ok {
		return
	}
	now := time.Now().UTC()
	u.LastLoginAt = &now
	u.UpdatedAt = now
	_ = r.put(ctx, PrefixUser+id, u)
}

func (r *Repository) DeleteUser(ctx context.Context, id string) error {
	if _, ok := r.LookupUser(id); !ok {
		return fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	// 连带删除该用户的会话
	r.mu.RLock()
	var tokens []string
	for hash, s := range r.sessions {
		if s.UserID == id {
			tokens = append(tokens, hash)
		}
	}
	r.mu.RUnlock()
	for _, tok := range tokens {
		_ = r.delete(ctx, PrefixSession+tok)
	}
	return r.delete(ctx, PrefixUser+id)
}

// CreateAPIKey 新建 API key,返回明文(仅此一次)。
func (r *Repository) CreateAPIKey(ctx context.Context, k *APIKey) (string, error) {
	if k.Status == "" {
		k.Status = StatusActive
	}
	if err := ValidateAPIKey(k); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	project, ok := r.GetProject(k.ProjectID)
	if !ok {
		return "", fmt.Errorf("%w: project %s", ErrNotFound, k.ProjectID)
	}
	if project.TenantID != k.TenantID {
		return "", fmt.Errorf("%w: project %s does not belong to tenant %s", ErrInvalid, k.ProjectID, k.TenantID)
	}
	plaintext, hash, prefix, err := NewAPIKey()
	if err != nil {
		return "", err
	}
	if k.ID == "" {
		k.ID = "key-" + randomSuffix()
	}
	k.KeyHash, k.KeyPrefix = hash, prefix
	now := time.Now().UTC()
	k.CreatedAt, k.UpdatedAt = now, now
	if err := r.put(ctx, PrefixAPIKey+k.ID, k); err != nil {
		return "", err
	}
	if err := r.verifyStoredKeyHash(ctx, k.ID, hash); err != nil {
		return "", err
	}
	return plaintext, nil
}

// RotateAPIKey 轮换密钥:保留配置(限额/白名单),换掉密钥本体。
func (r *Repository) RotateAPIKey(ctx context.Context, id string) (*APIKey, string, error) {
	current, ok := r.GetAPIKey(id)
	if !ok {
		return nil, "", fmt.Errorf("%w: apikey %s", ErrNotFound, id)
	}
	plaintext, hash, prefix, err := NewAPIKey()
	if err != nil {
		return nil, "", err
	}
	current.KeyHash, current.KeyPrefix = hash, prefix
	current.UpdatedAt = time.Now().UTC()
	if err := r.put(ctx, PrefixAPIKey+id, current); err != nil {
		return nil, "", err
	}
	if err := r.verifyStoredKeyHash(ctx, id, hash); err != nil {
		return nil, "", err
	}
	return current, plaintext, nil
}

// UpdateAPIKey 更新 key 的配置字段(不改密钥本体)。
func (r *Repository) UpdateAPIKey(ctx context.Context, id string, apply func(*APIKey)) (*APIKey, error) {
	current, ok := r.GetAPIKey(id)
	if !ok {
		return nil, fmt.Errorf("%w: apikey %s", ErrNotFound, id)
	}
	hash := current.KeyHash
	apply(current)
	current.ID = id
	current.KeyHash = hash
	current.UpdatedAt = time.Now().UTC()
	if err := ValidateAPIKey(current); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return current, r.put(ctx, PrefixAPIKey+id, current)
}

func (r *Repository) DeleteAPIKey(ctx context.Context, id string) error {
	if _, ok := r.GetAPIKey(id); !ok {
		return fmt.Errorf("%w: apikey %s", ErrNotFound, id)
	}
	return r.delete(ctx, PrefixAPIKey+id)
}

// TouchAPIKey 记录最后使用时间。异步、尽力而为,不阻塞数据面。
func (r *Repository) TouchAPIKey(ctx context.Context, id string) {
	k, ok := r.GetAPIKey(id)
	if !ok {
		return
	}
	now := time.Now().UTC()
	if k.LastUsedAt != nil && now.Sub(*k.LastUsedAt) < time.Minute {
		return // 一分钟内不重复写
	}
	k.LastUsedAt = &now
	k.UpdatedAt = now
	_ = r.put(ctx, PrefixAPIKey+id, k)
}

// ---------- 会话 ----------

// CreateSession 建立登录会话,返回明文 token(仅此一次)。
func (r *Repository) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, *Session, error) {
	if _, ok := r.LookupUser(userID); !ok {
		return "", nil, fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	plaintext, hash, err := NewSessionToken()
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	s := &Session{UserID: userID, CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now}
	if err := r.put(ctx, PrefixSession+hash, s); err != nil {
		return "", nil, err
	}
	return plaintext, s, nil
}

// DeleteSession 登出。
func (r *Repository) DeleteSession(ctx context.Context, tokenHash string) error {
	return r.delete(ctx, PrefixSession+tokenHash)
}

// DeleteUserSessions 撤销某用户全部会话(改密/禁用时用)。
func (r *Repository) DeleteUserSessions(ctx context.Context, userID string) error {
	r.mu.RLock()
	var hashes []string
	for hash, s := range r.sessions {
		if s.UserID == userID {
			hashes = append(hashes, hash)
		}
	}
	r.mu.RUnlock()
	for _, h := range hashes {
		if err := r.delete(ctx, PrefixSession+h); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) delete(ctx context.Context, key string) error {
	if r.js == nil {
		return fmt.Errorf("tenancy storage unavailable")
	}
	if err := r.js.Delete(ctx, key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return r.Refresh(ctx)
}

// HasAnyUser 用于首次启动引导判断。
func (r *Repository) HasAnyUser() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users) > 0
}

// Stats 返回各资源数量(给前端概览用)。
func (r *Repository) Stats() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return map[string]int{
		"tenants":  len(r.tenants),
		"projects": len(r.projects),
		"users":    len(r.users),
		"keys":     len(r.keys),
		"sessions": len(r.sessions),
	}
}
