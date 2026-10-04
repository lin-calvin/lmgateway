package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Middleware is intentionally small: an empty key disables auth for local
// development; a configured key protects API and OpenAI-compatible routes.
type Middleware struct {
	key string
}

func New(key string) *Middleware { return &Middleware{key: strings.TrimSpace(key)} }

func (m *Middleware) Enabled() bool { return m.key != "" }

func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Enabled() || publicPath(r.URL.Path) || m.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="lmgateway"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "authentication required"})
	})
}

func (m *Middleware) valid(r *http.Request) bool {
	provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if provided == "" {
		provided = bearerToken(r.Header.Get("Authorization"))
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(m.key)) == 1
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return ""
}

func publicPath(path string) bool {
	return path == "/healthz"
}
