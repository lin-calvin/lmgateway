package tenancy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
)

func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	repo, err := NewRepository(context.Background(), mem.NewJSON())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	return repo
}

func seedTenantProject(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, &Tenant{ID: "acme", Name: "Acme"}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := repo.CreateProject(ctx, &Project{ID: "web", TenantID: "acme", Name: "Web"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, "correct horse battery") {
		t.Fatal("hash must not contain the plaintext")
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("VerifyPassword should accept the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("VerifyPassword must reject a wrong password")
	}
	if VerifyPassword("garbage", "x") {
		t.Fatal("malformed hash must not verify")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short passwords must be rejected")
	}
}

func TestAPIKeyGenerationUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		plaintext, hash, prefix, err := NewAPIKey()
		if err != nil {
			t.Fatalf("NewAPIKey: %v", err)
		}
		if seen[plaintext] {
			t.Fatal("duplicate API key generated")
		}
		seen[plaintext] = true
		if !LooksLikeAPIKey(plaintext) {
			t.Fatalf("key %q does not carry the expected prefix", plaintext)
		}
		if hash == plaintext || strings.Contains(hash, plaintext) {
			t.Fatal("stored hash must not contain the plaintext key")
		}
		if !strings.Contains(prefix, "…") {
			t.Fatalf("display prefix should be truncated, got %q", prefix)
		}
	}
}

func TestModelAllowDeny(t *testing.T) {
	cases := []struct {
		name    string
		key     APIKey
		model   string
		allowed bool
	}{
		{"empty allow means all", APIKey{}, "anything", true},
		{"allow exact", APIKey{AllowedModels: []string{"gpt-4o"}}, "gpt-4o", true},
		{"allow mismatch", APIKey{AllowedModels: []string{"gpt-4o"}}, "gpt-4o-mini", false},
		{"allow wildcard", APIKey{AllowedModels: []string{"gpt-*"}}, "gpt-4o-mini", true},
		{"allow wildcard miss", APIKey{AllowedModels: []string{"gpt-*"}}, "claude-3", false},
		{"deny wins over allow-all", APIKey{DeniedModels: []string{"gpt-4o"}}, "gpt-4o", false},
		{"deny wins over allow", APIKey{AllowedModels: []string{"gpt-*"}, DeniedModels: []string{"gpt-4o"}}, "gpt-4o", false},
		{"deny wildcard keeps others", APIKey{AllowedModels: []string{"gpt-*"}, DeniedModels: []string{"gpt-4o"}}, "gpt-4o-mini", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.AllowsModel(tc.model); got != tc.allowed {
				t.Fatalf("AllowsModel(%q) = %v, want %v", tc.model, got, tc.allowed)
			}
		})
	}
}

func TestRepositoryLifecycle(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)

	key := &APIKey{TenantID: "acme", ProjectID: "web", Name: "prod"}
	secret, err := repo.CreateAPIKey(ctx, key)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if _, ok := repo.LookupAPIKey(HashSecret(secret)); !ok {
		t.Fatal("key lookup by hash failed")
	}
	if _, ok := repo.LookupAPIKey(HashSecret("sk-lm-bogus")); ok {
		t.Fatal("bogus key must not resolve")
	}
	// 列表接口不能泄露 hash
	for _, item := range repo.ListAPIKeys("acme", "") {
		if item.KeyHash != "" {
			t.Fatal("ListAPIKeys must not expose the key hash")
		}
	}

	// 项目归属校验:key 不能挂到别的租户的项目上
	if _, err := repo.CreateAPIKey(ctx, &APIKey{TenantID: "other", ProjectID: "web"}); err == nil {
		t.Fatal("cross-tenant project reference must be rejected")
	}

	// 轮换:旧 key 立即失效,新 key 可用
	rotated, newSecret, err := repo.RotateAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	if _, ok := repo.LookupAPIKey(HashSecret(secret)); ok {
		t.Fatal("old secret must stop working after rotation")
	}
	if _, ok := repo.LookupAPIKey(HashSecret(newSecret)); !ok {
		t.Fatal("new secret must work after rotation")
	}
	if rotated.KeyPrefix == "" {
		t.Fatal("rotated key should expose a display prefix")
	}

	// 有下级资源时禁止删租户
	if err := repo.DeleteTenant(ctx, "acme"); err == nil {
		t.Fatal("deleting a tenant with projects must fail")
	}
	if err := repo.DeleteProject(ctx, "web"); err == nil {
		t.Fatal("deleting a project with keys must fail")
	}
	if err := repo.DeleteAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if err := repo.DeleteProject(ctx, "web"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := repo.DeleteTenant(ctx, "acme"); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
}

func TestUserAndSession(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)

	user := &User{Email: "Owner@Acme.IO", Role: RoleTenantAdmin, TenantID: "acme"}
	if err := repo.CreateUser(ctx, user, "hunter2hunter2"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != "owner@acme.io" {
		t.Fatalf("email should be normalized, got %q", user.Email)
	}
	if _, ok := repo.LookupUserByEmail("OWNER@acme.io"); !ok {
		t.Fatal("email lookup must be case-insensitive")
	}
	stored, _ := repo.LookupUser(user.ID)
	if stored.PasswordHash == "" || VerifyPassword(stored.PasswordHash, "hunter2hunter2") == false {
		t.Fatal("stored password hash is not verifiable")
	}

	token, session, err := repo.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !LooksLikeSession(token) {
		t.Fatal("session token prefix mismatch")
	}
	if _, ok := repo.LookupSession(HashSecret(token)); !ok {
		t.Fatal("session lookup failed")
	}
	if session.Expired(time.Now()) {
		t.Fatal("fresh session must not be expired")
	}

	// 改密必须踢掉已有会话
	if err := repo.SetPassword(ctx, user.ID, "brandnewpass1"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := repo.DeleteUserSessions(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}
	if _, ok := repo.LookupSession(HashSecret(token)); ok {
		t.Fatal("session must be invalidated after password change")
	}
}

func TestLimiterRPMAndTPM(t *testing.T) {
	now := time.Now()
	limiter := NewLimiter(Options{Now: func() time.Time { return now }})
	identity := &Identity{Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", ProjectID: "web", RPMLimit: 2, TPMLimit: 100}

	for i := 0; i < 2; i++ {
		res, denial := limiter.Reserve(identity, 0, 10)
		if denial != nil {
			t.Fatalf("request %d should be allowed, denied: %s", i+1, denial.Message)
		}
		res.Settle(10, 0)
	}
	if _, denial := limiter.Reserve(identity, 0, 10); denial == nil || denial.Code != ReasonRPM {
		t.Fatalf("third request must hit the RPM limit, got %+v", denial)
	}

	// 下一个分钟窗口自动恢复
	now = now.Add(61 * time.Second)
	res, denial := limiter.Reserve(identity, 0, 10)
	if denial != nil {
		t.Fatalf("window rollover should reset counters, denied: %s", denial.Message)
	}
	res.Settle(95, 0)
	// TPM:已用 95 + 本次 10 > 100
	if _, denial := limiter.Reserve(identity, 0, 10); denial == nil || denial.Code != ReasonTPM {
		t.Fatalf("TPM limit must trigger, got %+v", denial)
	}
}

func TestLimiterCostReservation(t *testing.T) {
	limiter := NewLimiter(Options{})
	identity := &Identity{Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", ProjectID: "web", DailyCostLimit: 1.0}

	// 预扣 0.8 通过
	res, denial := limiter.Reserve(identity, 0.8, 0)
	if denial != nil {
		t.Fatalf("first reservation should pass, got %s", denial.Message)
	}
	// 在途预扣必须计入:0.8 + 0.3 > 1.0 → 拒绝
	if _, denial := limiter.Reserve(identity, 0.3, 0); denial == nil || denial.Code != ReasonDailyCost {
		t.Fatalf("in-flight reservation must block overspend, got %+v", denial)
	}
	// 结算实际只花了 0.1,预扣释放后又能放行
	res.Settle(0, 0.1)
	if _, denial := limiter.Reserve(identity, 0.5, 0); denial != nil {
		t.Fatalf("after settle there should be room, got %s", denial.Message)
	}

	// 快照反映真实用量
	limiter2 := NewLimiter(Options{})
	id2 := &Identity{Kind: IdentityAPIKey, KeyID: "k2"}
	res2, _ := limiter2.Reserve(id2, 0, 0)
	res2.Settle(42, 0.25)
	snap := limiter2.Snapshot("k2", "", "")
	if snap.TPMUsed != 42 {
		t.Fatalf("expected 42 tokens recorded, got %d", snap.TPMUsed)
	}
	if snap.DailyCost != 0.25 {
		t.Fatalf("expected 0.25 cost recorded, got %v", snap.DailyCost)
	}
}

func TestLimiterUnlimitedIdentity(t *testing.T) {
	limiter := NewLimiter(Options{})
	// master 身份不受配额约束
	res, denial := limiter.Reserve(&Identity{Kind: IdentityMaster}, 1e9, 1e9)
	if denial != nil {
		t.Fatalf("master identity must bypass quotas, got %s", denial.Message)
	}
	res.Settle(0, 0)
	if _, denial := limiter.Reserve(nil, 1, 1); denial != nil {
		t.Fatal("nil identity must bypass quotas")
	}
}

func TestAuthenticateCredentialScopes(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)
	key := &APIKey{TenantID: "acme", ProjectID: "web", Name: "prod"}
	secret, err := repo.CreateAPIKey(ctx, key)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	user := &User{Email: "admin@acme.io", Role: RoleAdmin}
	if err := repo.CreateUser(ctx, user, "adminpassword"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := repo.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	auth := NewAuthenticator("master-secret", repo)

	// master key
	identity, err := auth.Authenticate(request("master-secret"))
	if err != nil || identity.Kind != IdentityMaster || !identity.IsAdmin() {
		t.Fatalf("master key authentication failed: %+v %v", identity, err)
	}
	// API key → 数据面身份
	identity, err = auth.Authenticate(request(secret))
	if err != nil || identity.Kind != IdentityAPIKey {
		t.Fatalf("api key authentication failed: %+v %v", identity, err)
	}
	if identity.TenantID != "acme" || identity.ProjectID != "web" {
		t.Fatalf("api key identity lost its scope: %+v", identity)
	}
	if !ScopeData.allows(identity.Kind) || ScopeAdmin.allows(identity.Kind) {
		t.Fatal("API keys must be valid for the data plane only")
	}
	// session → 管理面身份
	identity, err = auth.Authenticate(request(token))
	if err != nil || identity.Kind != IdentityUser || identity.Role != RoleAdmin {
		t.Fatalf("session authentication failed: %+v %v", identity, err)
	}
	if !ScopeAdmin.allows(identity.Kind) || ScopeData.allows(identity.Kind) {
		t.Fatal("sessions must be valid for the admin plane only")
	}
	// 垃圾凭据
	if _, err := auth.Authenticate(request("nonsense")); err == nil {
		t.Fatal("unknown credential must not authenticate")
	}
	// 无凭据
	if _, err := auth.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("missing credential must not authenticate")
	}
}

func TestGuardRejectsCrossPlaneCredential(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)
	key := &APIKey{TenantID: "acme", ProjectID: "web"}
	secret, err := repo.CreateAPIKey(ctx, key)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	auth := NewAuthenticator("master-secret", repo)

	handler := auth.Guard(ScopeAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// 拿数据面 key 调管理面 → 403
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(secret))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("data key on admin plane should be 403, got %d", rec.Code)
	}
	// 无凭据 → 401
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing credential should be 401, got %d", rec.Code)
	}
	// 健康检查始终公开
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz must stay public, got %d", rec.Code)
	}
}

func TestEstimateHelpers(t *testing.T) {
	req := map[string]any{"max_tokens": float64(1000), "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	if got := EstimateOutputTokens(req, 4096); got != 1000 {
		t.Fatalf("EstimateOutputTokens = %d, want 1000", got)
	}
	if got := EstimateOutputTokens(map[string]any{}, 512); got != 512 {
		t.Fatalf("fallback = %d, want 512", got)
	}
	if got := EstimateInputTokens(req); got <= 0 {
		t.Fatalf("EstimateInputTokens = %d, want > 0", got)
	}
}

func TestLimiterMonthlyBudget(t *testing.T) {
	limiter := NewLimiter(Options{})
	identity := &Identity{Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", MonthlyCostLimit: 1.0}
	res, denial := limiter.Reserve(identity, 0.9, 0)
	if denial != nil {
		t.Fatalf("first reservation should pass, got %s", denial.Message)
	}
	if _, denial := limiter.Reserve(identity, 0.2, 0); denial == nil || denial.Code != ReasonMonthlyCost {
		t.Fatalf("monthly budget must block overspend, got %+v", denial)
	}
	res.Settle(0, 0)
	// 结算后月用量 0,预扣释放,又能放行
	if _, denial := limiter.Reserve(identity, 0.5, 0); denial != nil {
		t.Fatalf("after settle there should be room, got %s", denial.Message)
	}
}

// TestCalibrateFromSpendRecords 证明"重启不能绕过日/月预算":
// 计数器是进程内的,但启动时会从 spend 记录重建。
func TestCalibrateFromSpendRecords(t *testing.T) {
	backend := mem.NewTSBackend()
	ts := store.NewTS(backend, store.BufferedOpts{})
	ctx := context.Background()
	now := time.Now().UTC()

	if err := ts.Append(ctx, &store.TSRecord{
		Ts:     now,
		Stream: "spend",
		Tags:   map[string]string{"tenant": "acme", "project": "web", "key": "k1"},
		Fields: map[string]any{"cost": 0.5, "total_tokens": float64(100)},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	limiter := NewLimiter(Options{})
	if err := Calibrate(ctx, ts, limiter); err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	snapshot := limiter.Snapshot("k1", "web", "acme")
	if snapshot.DailyCost < 0.5 {
		t.Fatalf("daily cost should be calibrated to 0.5, got %v", snapshot.DailyCost)
	}
	if snapshot.MonthlyCost < 0.5 {
		t.Fatalf("monthly cost should be calibrated to 0.5, got %v", snapshot.MonthlyCost)
	}

	// 校准过的用量必须计入预算判定
	identity := &Identity{Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", ProjectID: "web", DailyCostLimit: 1.0}
	if _, denial := limiter.Reserve(identity, 0.6, 0); denial == nil || denial.Code != ReasonDailyCost {
		t.Fatalf("calibrated usage must count toward the budget, got %+v", denial)
	}
}

func TestGuardConfigRequiresGlobalAdmin(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)

	mkUser := func(email, role, tenantID, password string) string {
		t.Helper()
		u := &User{Email: email, Role: role, TenantID: tenantID}
		if err := repo.CreateUser(ctx, u, password); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
		token, _, err := repo.CreateSession(ctx, u.ID, time.Hour)
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", email, err)
		}
		return token
	}

	adminToken := mkUser("root@example.com", RoleAdmin, "", "adminpassword")
	tenantAdminToken := mkUser("ta@acme.io", RoleTenantAdmin, "acme", "tenantpass1")
	memberToken := mkUser("member@acme.io", RoleMember, "acme", "memberpass1")

	secret, err := repo.CreateAPIKey(ctx, &APIKey{TenantID: "acme", ProjectID: "web"})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	authn := NewAuthenticator("master-secret", repo)
	handler := authn.GuardConfig(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"无凭据", "", http.StatusUnauthorized},
		{"master key", "master-secret", http.StatusOK},
		{"全局管理员会话", adminToken, http.StatusOK},
		{"租户管理员会话 → 403", tenantAdminToken, http.StatusForbidden},
		{"成员会话 → 403", memberToken, http.StatusForbidden},
		{"数据面 API key → 403", secret, http.StatusForbidden},
		{"垃圾凭据", "nonsense", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/provider/list", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestGuardConfigOpenModeOnlyWhenUnconfigured(t *testing.T) {
	ctx := context.Background()

	// 1) 无 master key 且库里没有用户 → 放行(保留本地开发体验),并注入身份
	empty := newTestRepo(t)
	authn := NewAuthenticator("", empty)
	sawIdentity := false
	handler := authn.GuardConfig(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawIdentity = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/provider/list", nil))
	if rec.Code != http.StatusOK || !sawIdentity {
		t.Fatalf("未配置时应放行并注入身份, got %d identity=%v", rec.Code, sawIdentity)
	}

	// 2) 一旦库里有了用户,即使没配 master key 也必须登录
	populated := newTestRepo(t)
	if err := populated.CreateUser(ctx, &User{Email: "root@example.com", Role: RoleAdmin}, "adminpassword"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	authn2 := NewAuthenticator("", populated)
	rec = httptest.NewRecorder()
	authn2.GuardConfig(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/provider/list", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("已有用户时应要求凭据, got %d", rec.Code)
	}
}

// TestConcurrentCreateWithRefreshKeepsIndexFresh 覆盖一个真实竞态：
// 每次写入都会重建全量索引，而 Watch 回调与定期兜底也会重建。若两次装载交叠、
// "先启动后完成"的那次把旧快照盖上去，刚创建的密钥就会**库里存在、索引里没有**
// —— 客户端拿到了 201 和明文，紧接着却 401。
//
// 这里做白盒断言：不依赖回源兜底，直接检查索引里必须有该密钥。
func TestConcurrentCreateWithRefreshKeepsIndexFresh(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	seedTenantProject(t, repo)

	const n = 24
	secrets := make([]string, n)
	errs := make([]error, n)

	// 后台持续刷新，制造与写入的交叠
	stop := make(chan struct{})
	var refresher sync.WaitGroup
	refresher.Add(1)
	go func() {
		defer refresher.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = repo.Refresh(ctx)
			}
		}
	}()

	var creators sync.WaitGroup
	for i := 0; i < n; i++ {
		creators.Add(1)
		go func(i int) {
			defer creators.Done()
			secret, err := repo.CreateAPIKey(ctx, &APIKey{
				TenantID: "acme", ProjectID: "web", Name: fmt.Sprintf("k%d", i),
			})
			secrets[i], errs[i] = secret, err
		}(i)
	}
	creators.Wait()
	close(stop)
	refresher.Wait()

	for i := range secrets {
		if errs[i] != nil {
			t.Fatalf("create %d failed: %v", i, errs[i])
		}
		hash := HashSecret(secrets[i])
		repo.mu.RLock()
		_, inIndex := repo.keyByHash[hash]
		repo.mu.RUnlock()
		if !inIndex {
			t.Fatalf("key %d 已创建但不在内存索引里（旧快照赢得了换入）", i)
		}
		if _, ok := repo.LookupAPIKey(hash); !ok {
			t.Fatalf("key %d 创建后无法立即鉴权", i)
		}
	}
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
