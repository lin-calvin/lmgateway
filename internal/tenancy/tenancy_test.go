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

// TestTenantAndProjectLimitsAreEnforced 证明三层配额都真的会拦截：
// key 没超但 project/tenant 超了，同样必须拒绝，并且拒绝要指名是哪一层。
func TestTenantAndProjectLimitsAreEnforced(t *testing.T) {
	now := time.Now()
	newScopedLimiter := func() *Limiter {
		return NewLimiter(Options{Now: func() time.Time { return now }})
	}
	scopedIdentity := func() *Identity {
		return &Identity{
			Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", ProjectID: "web",
			QuotaScopes: []QuotaScope{
				{Scope: "key:k1", Label: "API key prod (sk-lm-1)"},
				{Scope: "project:web", Label: "project web", RPMLimit: 2},
				{Scope: "tenant:acme", Label: "tenant acme"},
			},
		}
	}

	// project 层 RPM=2：第三次请求必须在 project 层被拒
	limiter := newScopedLimiter()
	id := scopedIdentity()
	for i := 0; i < 2; i++ {
		res, denial := limiter.Reserve(id, 0, 0)
		if denial != nil {
			t.Fatalf("request %d should pass, denied: %s", i+1, denial.Message)
		}
		res.Settle(0, 0)
	}
	_, denial := limiter.Reserve(id, 0, 0)
	if denial == nil || denial.Code != ReasonRPM {
		t.Fatalf("project-level RPM must deny, got %+v", denial)
	}
	if denial.Scope != "project:web" || denial.Label != "project web" {
		t.Fatalf("denial must name the layer that hit the limit, got scope=%q label=%q", denial.Scope, denial.Label)
	}
	if !strings.Contains(denial.Message, "project web") {
		t.Fatalf("denial message should be actionable, got %q", denial.Message)
	}

	// tenant 层日预算=1.0：key 没有限额，仍必须被租户预算拦住
	limiter2 := newScopedLimiter()
	id2 := scopedIdentity()
	id2.QuotaScopes = []QuotaScope{
		{Scope: "key:k1", Label: "API key prod (sk-lm-1)"},
		{Scope: "project:web", Label: "project web"},
		{Scope: "tenant:acme", Label: "tenant acme", DailyCostLimit: 1.0},
	}
	res, denial := limiter2.Reserve(id2, 0.8, 0)
	if denial != nil {
		t.Fatalf("first reservation should pass, got %s", denial.Message)
	}
	if _, denial := limiter2.Reserve(id2, 0.3, 0); denial == nil || denial.Code != ReasonDailyCost {
		t.Fatalf("tenant-level daily budget must deny, got %+v", denial)
	} else if denial.Scope != "tenant:acme" {
		t.Fatalf("denial scope = %q, want tenant:acme", denial.Scope)
	}
	res.Settle(0, 0.1)
	if _, denial := limiter2.Reserve(id2, 0.5, 0); denial != nil {
		t.Fatalf("after settle there should be room, got %s", denial.Message)
	}

	// 月预算同理
	limiter3 := newScopedLimiter()
	id3 := scopedIdentity()
	id3.QuotaScopes = []QuotaScope{{Scope: "tenant:acme", Label: "tenant acme", MonthlyCostLimit: 2.0}}
	if _, denial := limiter3.Reserve(id3, 2.5, 0); denial == nil || denial.Code != ReasonMonthlyCost {
		t.Fatalf("tenant-level monthly budget must deny, got %+v", denial)
	}
}

// TestTenantLimitsSharedAcrossKeys 证明租户预算是**合计**预算：
// 同一个租户下的两把不同 key 共同消耗同一个租户额度。
func TestTenantLimitsSharedAcrossKeys(t *testing.T) {
	limiter := NewLimiter(Options{})
	tenantScope := func(keyID string) *Identity {
		return &Identity{
			Kind: IdentityAPIKey, TenantID: "acme", ProjectID: "web", KeyID: keyID,
			QuotaScopes: []QuotaScope{
				{Scope: "key:" + keyID, Label: "key " + keyID},
				{Scope: "tenant:acme", Label: "tenant acme", DailyCostLimit: 1.0},
			},
		}
	}
	res, denial := limiter.Reserve(tenantScope("k1"), 0.6, 0)
	if denial != nil {
		t.Fatalf("k1 first request should pass: %s", denial.Message)
	}
	res.Settle(0, 0.6)
	// 另一把 key：它自己没有限额，但租户已经用了 0.6，再来 0.6 必须被拒
	if _, denial := limiter.Reserve(tenantScope("k2"), 0.6, 0); denial == nil || denial.Code != ReasonDailyCost {
		t.Fatalf("tenant budget must be shared across keys, got %+v", denial)
	}
}

// TestAuthenticatorPopulatesQuotaScopes 证明鉴权时会把三层限额都取出来。
func TestAuthenticatorPopulatesQuotaScopes(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, &Tenant{ID: "acme", Name: "Acme", DailyCostLimit: 5}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateProject(ctx, &Project{ID: "web", TenantID: "acme", Name: "Web", RPMLimit: 600}); err != nil {
		t.Fatal(err)
	}
	key := &APIKey{TenantID: "acme", ProjectID: "web", Name: "prod", MonthlyCostLimit: 100}
	secret, err := repo.CreateAPIKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuthenticator("master-secret", repo)
	identity, err := auth.Authenticate(request(secret))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	byScope := map[string]QuotaScope{}
	for _, sc := range identity.QuotaScopes {
		byScope[sc.Scope] = sc
	}
	if len(byScope) != 3 {
		t.Fatalf("expected key/project/tenant scopes, got %+v", identity.QuotaScopes)
	}
	if byScope["key:"+key.ID].MonthlyCostLimit != 100 {
		t.Fatalf("key limits missing: %+v", byScope["key:"+key.ID])
	}
	if byScope["project:web"].RPMLimit != 600 {
		t.Fatalf("project limits missing: %+v", byScope["project:web"])
	}
	if byScope["tenant:acme"].DailyCostLimit != 5 {
		t.Fatalf("tenant limits missing: %+v", byScope["tenant:acme"])
	}

	// 鉴权拿到的身份直接喂给限流器，租户预算必须真的生效
	limiter := NewLimiter(Options{})
	res, denial := limiter.Reserve(identity, 4.0, 0)
	if denial != nil {
		t.Fatalf("first request should pass: %s", denial.Message)
	}
	res.Settle(0, 4.0)
	if _, denial := limiter.Reserve(identity, 2.0, 0); denial == nil || denial.Scope != "tenant:acme" {
		t.Fatalf("authenticated identity must enforce tenant budget, got %+v", denial)
	}
}

// TestSnapshotReportsEveryScope 证明快照能区分 key / project / tenant 三层用量。
func TestSnapshotReportsEveryScope(t *testing.T) {
	limiter := NewLimiter(Options{})
	identity := &Identity{
		Kind: IdentityAPIKey, KeyID: "k1", TenantID: "acme", ProjectID: "web",
		QuotaScopes: []QuotaScope{
			{Scope: "key:k1", Label: "key k1"},
			{Scope: "project:web", Label: "project web"},
			{Scope: "tenant:acme", Label: "tenant acme"},
		},
	}
	res, denial := limiter.Reserve(identity, 0.25, 0)
	if denial != nil {
		t.Fatalf("Reserve: %s", denial.Message)
	}
	res.Settle(42, 0.25)

	snap := limiter.Snapshot("k1", "web", "acme")
	if snap.TPMUsed != 42 || snap.DailyCost != 0.25 {
		t.Fatalf("key-level snapshot wrong: %+v", snap)
	}
	if len(snap.Scopes) != 3 {
		t.Fatalf("expected 3 scopes in snapshot, got %+v", snap.Scopes)
	}
	for _, usage := range snap.Scopes {
		// 三层都按全额记账：token 与成本在每一层都可见
		if usage.TPMUsed != 42 || usage.DailyCost != 0.25 {
			t.Fatalf("scope %s should see full usage, got %+v", usage.Scope, usage)
		}
	}
}

func TestQuotaScopeValidation(t *testing.T) {
	if err := ValidateTenant(&Tenant{ID: "acme", Name: "Acme", RPMLimit: -1}); err == nil {
		t.Fatal("negative tenant rpm limit must be rejected")
	}
	if err := ValidateProject(&Project{ID: "web", TenantID: "acme", Name: "Web", DailyCostLimit: -0.5}); err == nil {
		t.Fatal("negative project cost limit must be rejected")
	}
	if err := ValidateTenant(&Tenant{ID: "acme", Name: "Acme", Status: StatusActive, RPMLimit: 100, MonthlyCostLimit: 5}); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
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
	if err := Calibrate(ctx, ts, limiter, nil); err != nil {
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

// TestCalibrateIncludesPrunedHistory 证明月中重启时,被 rollup 裁剪掉的历史
// (只存在于 spend_daily)同样计入月度预算 —— 否则重启即可凭空恢复预算。
func TestCalibrateIncludesPrunedHistory(t *testing.T) {
	backend := mem.NewTSBackend()
	ts := store.NewTS(backend, store.BufferedOpts{})
	ctx := context.Background()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// 历史:已经被汇总进 spend_daily、raw 已删除
	older := now.AddDate(0, 0, -3)
	if older.Before(monthStart) {
		older = monthStart
	}
	if err := ts.Append(ctx, &store.TSRecord{
		Ts:     older,
		Stream: "spend_daily",
		Tags:   map[string]string{"day": older.Format("2006-01-02"), "tenant": "acme", "project": "web", "key": "k1"},
		Fields: map[string]any{"cost": 2.0, "requests": float64(3)},
	}); err != nil {
		t.Fatalf("Append daily: %v", err)
	}
	// 近期:仍是 raw
	if err := ts.Append(ctx, &store.TSRecord{
		Ts:     now.Add(-time.Minute),
		Stream: "spend",
		Tags:   map[string]string{"tenant": "acme", "project": "web", "key": "k1"},
		Fields: map[string]any{"cost": 0.5},
	}); err != nil {
		t.Fatalf("Append raw: %v", err)
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	watermark := func(context.Context) (time.Time, error) { return now.Add(-time.Hour), nil }
	limiter := NewLimiter(Options{})
	if err := Calibrate(ctx, ts, limiter, watermark); err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	snapshot := limiter.Snapshot("k1", "web", "acme")
	if snapshot.MonthlyCost < 2.5 {
		t.Fatalf("monthly cost must include pruned history (want >= 2.5), got %v", snapshot.MonthlyCost)
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
