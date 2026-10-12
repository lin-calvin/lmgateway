package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := New("secret", nil, nil).Wrap(next)

	for _, tc := range []struct {
		name   string
		header map[string]string
		path   string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", header: map[string]string{"Authorization": "Bearer bad"}, want: http.StatusUnauthorized},
		{name: "bearer", header: map[string]string{"Authorization": "Bearer secret"}, want: http.StatusNoContent},
		{name: "x api key", header: map[string]string{"X-API-Key": "secret"}, want: http.StatusNoContent},
		{name: "health public", path: "/healthz", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/api/private"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, w.Code)
			}
		})
	}
}

func TestDisabledMiddleware(t *testing.T) {
	h := New("", nil, nil).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodGet, "/api/private", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disabled auth should pass, got %d", w.Code)
	}
}

func TestAPIKeyLookupAndGroups(t *testing.T) {
	token := "sk-lm-testtoken"
	key := Key{ID: "k1", Groups: []string{"eng"}, Status: "active"}
	lookup := func(hash string) (Key, bool) {
		if hash == HashToken(token) {
			return key, true
		}
		return Key{}, false
	}
	var got *Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := New("master", lookup, func() bool { return true }).Wrap(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got == nil || got.Kind != "apikey" || got.ID != "k1" {
		t.Fatalf("unexpected principal: %+v", got)
	}
	// namesake group (id) is always appended
	if !hasGroup(got.Groups, "eng") || !hasGroup(got.Groups, "k1") {
		t.Fatalf("expected groups [eng k1], got %v", got.Groups)
	}

	// expired key is rejected
	key.ExpiresAt = ptrTime(time.Now().Add(-time.Hour))
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set("X-API-Key", token)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expired key should 401, got %d", w2.Code)
	}
}

func TestMasterKeyIsRoot(t *testing.T) {
	var got *Principal
	h := New("master", func(string) (Key, bool) { return Key{}, false }, func() bool { return true }).
		Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = FromContext(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got == nil || got.Kind != "root" || !hasGroup(got.Groups, "*") {
		t.Fatalf("expected root principal, got %+v", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func hasGroup(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}
