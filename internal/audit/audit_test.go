package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
	"lmgateway/internal/tenancy"
)

func newLogger(t *testing.T) (*Logger, store.JSONStore) {
	t.Helper()
	js := mem.NewJSON()
	l := New(js, Options{Now: func() time.Time { return time.Now().UTC() }})
	t.Cleanup(l.Close)
	return l, js
}

func adminCtx() context.Context {
	return tenancy.WithIdentity(context.Background(), &tenancy.Identity{
		Kind: tenancy.IdentityUser, UserID: "u1", Email: "ops@example.com", Role: tenancy.RoleAdmin,
	})
}

func TestRecordQueryAndPagination(t *testing.T) {
	l, _ := newLogger(t)
	ctx := adminCtx()

	for i := 0; i < 5; i++ {
		l.Record(ctx, &Entry{
			Action: "model.patch", Resource: "model/gpt-x",
			Detail: map[string]any{"changes": map[string]any{"input_per_mtok": map[string]any{"from": 1, "to": 2}}, "api_key": "sk-secret"},
		})
	}
	l.Record(ctx, &Entry{Action: "apikey.create", Resource: "apikey/k1", TenantID: "acme"})
	// 等一下异步写队列。
	waitFor(t, func() bool {
		r, err := l.Query(context.Background(), Query{})
		return err == nil && r.Total == 6
	})

	res, err := l.Query(context.Background(), Query{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 6 || len(res.Items) != 2 {
		t.Fatalf("total=%d len=%d, want 6/2", res.Total, len(res.Items))
	}
	// 最新在前：同时间戳时按 ID 倒序，但 Action 的集合必须覆盖全部。
	res2, _ := l.Query(context.Background(), Query{Limit: 10, Offset: 5})
	if len(res2.Items) != 1 {
		t.Fatalf("offset page len = %d, want 1", len(res2.Items))
	}

	// 过滤：action 子串。
	filtered, err := l.Query(context.Background(), Query{Action: "model.patch"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if filtered.Total != 5 {
		t.Fatalf("action filter total = %d, want 5", filtered.Total)
	}
	// 过滤：租户。
	byTenant, _ := l.Query(context.Background(), Query{TenantID: "acme"})
	if byTenant.Total != 1 {
		t.Fatalf("tenant filter total = %d, want 1", byTenant.Total)
	}
	// 敏感字段必须脱敏。
	if len(filtered.Items) == 0 {
		t.Fatal("no audit items")
	}
	if got := filtered.Items[0].Detail["api_key"]; got != "***" {
		t.Fatalf("api_key = %v, want ***", got)
	}
	// actor 从 context 身份解析。
	if filtered.Items[0].Actor.Email != "ops@example.com" {
		t.Fatalf("actor = %+v", filtered.Items[0].Actor)
	}
}

func TestMiddlewareRecordsMutationsAndSkipsReads(t *testing.T) {
	l, _ := newLogger(t)

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// 模拟处理器显式记录（比兜底更语义化）。
			Record(r.Context(), "apikey.rotate", "apikey/k1", map[string]any{"key_prefix": "sk-lm-abcd"})
		}
		w.WriteHeader(http.StatusOK)
	}))

	// GET 不留兜底记录。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenancy/keys", nil)
	req = req.WithContext(adminCtx())
	handler.ServeHTTP(rec, req)

	// POST 由处理器显式记录一次（中间件不重复补记）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/tenancy/keys/k1/rotate", nil)
	req = req.WithContext(adminCtx())
	handler.ServeHTTP(rec, req)

	// DELETE 走兜底记录。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/tenancy/keys/k1", nil)
	req = req.WithContext(adminCtx())
	handler.ServeHTTP(rec, req)

	waitFor(t, func() bool {
		r, err := l.Query(context.Background(), Query{})
		return err == nil && r.Total == 2
	})
	res, _ := l.Query(context.Background(), Query{})
	actions := map[string]int{}
	for _, e := range res.Items {
		actions[e.Action]++
	}
	if actions["apikey.rotate"] != 1 {
		t.Fatalf("explicit action count = %d, want 1 (actions=%v)", actions["apikey.rotate"], actions)
	}
	if actions["delete /api/tenancy/keys/k1"] != 1 {
		t.Fatalf("fallback action missing, actions=%v", actions)
	}
	if _, ok := actions["get /api/tenancy/keys"]; ok {
		t.Fatalf("GET must not be audited, actions=%v", actions)
	}
}

func TestDiffAndPrune(t *testing.T) {
	before := map[string]any{"input_per_mtok": 3.0, "output_per_mtok": 6.0, "tag": "keep"}
	after := map[string]any{"input_per_mtok": 5.0, "output_per_mtok": 6.0}
	diff := Diff(before, after)
	change, ok := diff["input_per_mtok"].(map[string]any)
	if !ok || change["from"] != 3.0 || change["to"] != 5.0 {
		t.Fatalf("diff = %#v", diff)
	}
	// 删除的字段也要记录。
	if _, ok := diff["tag"]; !ok {
		t.Fatalf("removed field missing from diff: %#v", diff)
	}
	// 未变化的不进 diff。
	if _, ok := diff["output_per_mtok"]; ok {
		t.Fatalf("unchanged field leaked into diff: %#v", diff)
	}

	js := mem.NewJSON()
	l := New(js, Options{PruneEvery: 1})
	defer l.Close()
	old := time.Now().UTC().AddDate(0, 0, -100)
	_, err := js.Put(context.Background(), Prefix+old.Format("2006-01-02")+"/old", &Entry{ID: "old", At: old, Action: "x"})
	if err != nil {
		t.Fatal(err)
	}
	l.Record(adminCtx(), &Entry{Action: "y"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := js.Get(context.Background(), Prefix+old.Format("2006-01-02")+"/old"); err == store.ErrNotFound {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("old audit entry was not pruned")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
