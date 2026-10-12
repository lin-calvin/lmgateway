package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Principal is the resolved identity for a request. It carries the groups used
// by the resource ACL. It never contains the raw token or secret.
type Principal struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // "root" | "apikey"
	Groups []string `json:"groups"`
}

// Key is the minimal credential view the middleware needs. The caller (config
// manager) supplies the concrete key records via a lookup function.
type Key struct {
	ID        string
	Groups    []string
	Status    string
	ExpiresAt *time.Time
}

func (k Key) active(now time.Time) bool {
	if k.Status == "disabled" {
		return false
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return false
	}
	return true
}

// Middleware authenticates requests: the configured master key maps to the root
// principal ("*" group, all access); API keys are looked up by sha256 and mapped
// to a principal whose groups are the key's groups plus a namesake group (the key
// id), so per-key grants work without a separate binding concept.
//
// When no credential is configured (no master key and no keys) auth is disabled
// for local development, matching the previous single-token behavior.
type Middleware struct {
	masterKey string
	lookup    func(tokenHash string) (Key, bool)
	enforced  func() bool
}

func New(masterKey string, lookup func(tokenHash string) (Key, bool), enforced func() bool) *Middleware {
	if lookup == nil {
		lookup = func(string) (Key, bool) { return Key{}, false }
	}
	if enforced == nil {
		enforced = func() bool { return strings.TrimSpace(masterKey) != "" }
	}
	return &Middleware{masterKey: strings.TrimSpace(masterKey), lookup: lookup, enforced: enforced}
}

func (m *Middleware) Enabled() bool { return m.enforced() }

func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !m.enforced() {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), rootPrincipal())))
			return
		}
		token := extractToken(r)
		if token == "" {
			writeAuthError(w, "authentication required")
			return
		}
		if m.masterKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.masterKey)) == 1 {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), rootPrincipal())))
			return
		}
		if k, ok := m.lookup(HashToken(token)); ok && k.active(time.Now()) {
			groups := append([]string(nil), k.Groups...)
			if k.ID != "" {
				groups = append(groups, k.ID) // namesake group
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), &Principal{Kind: "apikey", ID: k.ID, Groups: groups})))
			return
		}
		writeAuthError(w, "authentication required")
	})
}

func rootPrincipal() *Principal {
	return &Principal{Kind: "root", ID: "root", Groups: []string{"*"}}
}

// HashToken returns the hex sha256 of a presented token (the stored form).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func extractToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	return bearerToken(r.Header.Get("Authorization"))
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

func writeAuthError(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="lmgateway"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

type principalCtxKey struct{}

// WithPrincipal injects the resolved principal into the request context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// FromContext reads the resolved principal, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok && p != nil
}
