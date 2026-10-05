package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/api"
	"lmgateway/internal/audit"
	"lmgateway/internal/config"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
	"lmgateway/internal/store/mem"
	"lmgateway/internal/tenancy"
)

// 审计验收：改模型价要能查到"谁把哪个字段从 A 改成 B"，发密钥要能查到 key 前缀，
// 且明文/哈希/服务端密钥绝不落审计。
func TestConfigWritesAreAuditedWithActorAndDiff(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	ts := store.NewTS(mem.NewTSBackend(), store.BufferedOpts{BatchSize: 1000, Interval: time.Hour})
	seed := config.Config{
		Providers: []config.ProviderCfg{{Name: "openai", Type: "openai", BaseURL: "http://x", APIKey: "sk-upstream-secret"}},
		Models:    []config.ModelCfg{{Name: "gpt-4o", Provider: "openai", InputPerMtok: 3}},
	}
	m := config.NewManagerWithBase(js, ts, seed)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { ts.Close(); m.Close() }()

	auditLog := audit.New(js, audit.Options{})
	defer auditLog.Close()

	admin := &tenancy.Identity{
		Kind: tenancy.IdentityUser, UserID: "u1", Email: "ops@example.com", Role: tenancy.RoleAdmin,
	}
	inner := auditLog.Middleware(api.New(api.Deps{Manager: m, TS: ts, Rollup: rollup.New(ts, js)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(tenancy.WithIdentity(r.Context(), admin)))
	}))
	defer srv.Close()

	// 1) 改模型价
	body, _ := json.Marshal(map[string]any{"provider": "openai", "input_per_mtok": 5})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/model/patch/gpt-4o", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch model status = %d", resp.StatusCode)
	}

	// 2) 覆盖 provider（含上游 api_key），审计必须脱敏
	provBody, _ := json.Marshal(map[string]any{
		"name": "openai", "type": "openai", "base_url": "http://x", "api_key": "sk-upstream-secret2",
	})
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/api/provider/set/openai", bytes.NewReader(provBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 3) 读操作不留审计
	resp, err = http.Get(srv.URL + "/api/model/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	var entries []*audit.Entry
	for time.Now().Before(deadline) {
		res, err := auditLog.Query(ctx, audit.Query{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		entries = res.Items
		if len(entries) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	byAction := map[string]audit.Entry{}
	for _, e := range entries {
		byAction[e.Action] = *e
	}
	if _, ok := byAction["get /api/model/list"]; ok {
		t.Fatalf("GET must not be audited: %+v", byAction)
	}

	modelEntry, ok := byAction["model.patch"]
	if !ok {
		t.Fatalf("model.patch not audited; actions=%v", keys(byAction))
	}
	if modelEntry.Actor.Email != "ops@example.com" || modelEntry.Resource != "model/gpt-4o" {
		t.Fatalf("audit actor/resource = %+v", modelEntry)
	}
	changes, _ := modelEntry.Detail["changes"].(map[string]any)
	price, _ := changes["input_per_mtok"].(map[string]any)
	if price == nil || price["from"] != float64(3) || price["to"] != float64(5) {
		t.Fatalf("price diff = %#v", changes)
	}

	providerEntry, ok := byAction["provider.set"]
	if !ok {
		t.Fatalf("provider.set not audited; actions=%v", keys(byAction))
	}
	if raw := providerEntry.Detail["changes"]; raw != nil {
		changes, _ := raw.(map[string]any)
		if apiKey, ok := changes["api_key"].(map[string]any); ok {
			if apiKey["to"] != "***" || apiKey["from"] != "***" {
				t.Fatalf("api_key must be redacted in audit: %#v", apiKey)
			}
		}
	}
	if providerEntry.Actor.Email != "ops@example.com" {
		t.Fatalf("provider audit actor = %+v", providerEntry.Actor)
	}

	// 4) 删除模型：审计必须留下"被删掉的是什么"（before 快照要在删除前抓）
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/model/delete/gpt-4o", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete model status = %d", resp.StatusCode)
	}
	deadline = time.Now().Add(3 * time.Second)
	var deleteEntry *audit.Entry
	for time.Now().Before(deadline) {
		res, err := auditLog.Query(ctx, audit.Query{Action: "model.delete", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) > 0 {
			deleteEntry = res.Items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deleteEntry == nil {
		t.Fatal("model.delete was not audited")
	}
	deleted, _ := deleteEntry.Detail["changes"].(map[string]any)
	deletedPrice, _ := deleted["input_per_mtok"].(map[string]any)
	if deletedPrice == nil || deletedPrice["from"] != float64(5) {
		t.Fatalf("delete audit must keep the before-image, got %#v", deleted)
	}
}

func keys(m map[string]audit.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
