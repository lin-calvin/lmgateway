package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := New("secret").Wrap(next)

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
	h := New("").Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodGet, "/api/private", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disabled auth should pass, got %d", w.Code)
	}
}
