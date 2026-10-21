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

func TestKeySuffix(t *testing.T) {
	token := "sk-lm-tagtest"
	key := Key{ID: "k1", Groups: []string{"eng"}, Status: "active"}
	lookup := func(hash string) (Key, bool) {
		if hash == HashToken(token) {
			return key, true
		}
		return Key{}, false
	}
	var got *Principal
	var suffix string
	h := New("master", lookup, func() bool { return true }).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		suffix = KeySuffixFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(header string) int {
		got, suffix = nil, ""
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	if code := send("Bearer " + token + "#cli"); code != http.StatusNoContent {
		t.Fatalf("tagged key should authenticate, got %d", code)
	}
	if got == nil || got.ID != "k1" {
		t.Fatalf("unexpected principal: %+v", got)
	}
	if suffix != "cli" {
		t.Fatalf("suffix = %q, want cli", suffix)
	}
	if hasGroup(got.Groups, "cli") {
		t.Fatalf("suffix must never become a group: %v", got.Groups)
	}

	if code := send("Bearer " + token + "#cli&pool_policy=price_first"); code != http.StatusNoContent {
		t.Fatalf("tagged key with hints should authenticate, got %d", code)
	}
	if suffix != "cli&pool_policy=price_first" {
		t.Fatalf("suffix = %q", suffix)
	}

	// The suffix is opaque to auth; syntax is handled by the HTTP layer.
	if code := send("Bearer " + token + "#a&b"); code != http.StatusNoContent {
		t.Fatalf("arbitrary suffix should still authenticate, got %d", code)
	}
	if suffix != "a&b" {
		t.Fatalf("suffix = %q", suffix)
	}

	if code := send("Bearer master#root-tag"); code != http.StatusNoContent {
		t.Fatalf("tagged master key should authenticate, got %d", code)
	}
	if got == nil || got.Kind != "root" || suffix != "root-tag" {
		t.Fatalf("unexpected tagged master principal: %+v suffix=%q", got, suffix)
	}
}

func TestKeySuffixExactKeyWithHash(t *testing.T) {
	full := "sk-lm-a#b"
	lookup := func(hash string) (Key, bool) {
		if hash == HashToken(full) {
			return Key{ID: "weird"}, true
		}
		return Key{}, false
	}
	var suffix string
	h := New("", lookup, func() bool { return true }).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix = KeySuffixFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("exact key containing # should authenticate, got %d", w.Code)
	}
	if suffix != "" {
		t.Fatalf("exact key match must not infer a suffix, got %q", suffix)
	}
}

func TestDisabledMiddlewareReadsSuffix(t *testing.T) {
	var suffix string
	var principal *Principal
	h := New("", nil, nil).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix = KeySuffixFromContext(r.Context())
		principal, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer anything#cli")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || suffix != "cli" {
		t.Fatalf("open mode suffix: code=%d suffix=%q", w.Code, suffix)
	}
	if principal == nil || principal.Kind != "root" {
		t.Fatalf("open mode principal: %+v", principal)
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
