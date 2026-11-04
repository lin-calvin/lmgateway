package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"lmgateway/internal/auth"
	"lmgateway/internal/packet"
)

func TestCollectHTTPMetadataOpenCode(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "opencode/2")
	req.Header.Set("x-opencode-session", "ses_affinity")
	req.Header.Set("x-opencode-session-id", "ses_real")
	req.Header.Set("x-opencode-project", "proj_1")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-session-affinity", "ses_affinity")
	req.Header.Set("X-Session-Id", "ses_affinity")
	req.Header.Set("x-parent-session-id", "ses_parent")
	req.Header.Set("Authorization", "Bearer secret")

	meta := collectHTTPMetadata(req)
	if got := meta["session_id"]; got != "ses_real" {
		t.Fatalf("session_id = %v, want the OpenCode session id", got)
	}
	headers, ok := meta["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers missing: %v", meta)
	}
	for name, want := range map[string]string{
		"x-opencode-session":    "ses_affinity",
		"x-opencode-session-id": "ses_real",
		"x-opencode-project":    "proj_1",
		"x-opencode-client":     "cli",
		"x-session-affinity":    "ses_affinity",
		"x-session-id":          "ses_affinity",
		"x-parent-session-id":   "ses_parent",
	} {
		if got := headers[name]; got != want {
			t.Errorf("headers[%s] = %v, want %s", name, got, want)
		}
	}
	if _, ok := headers["authorization"]; ok {
		t.Fatal("authorization must not be collected")
	}
}

func ensureMeta(pkt packet.Packet) map[string]any {
	meta, _ := pkt.Map(packet.KeyHTTPMeta)
	headers, _ := meta["headers"].(map[string]any)
	return headers
}

func TestEnsureOpenCodeSession(t *testing.T) {
	// A client-provided session wins.
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyHTTPMeta, map[string]any{
		"headers": map[string]any{"x-opencode-session": "ses_client"},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureOpenCodeSession(pkt, req, &auth.Principal{ID: "k1"}, true)
	if got := ensureMeta(pkt)["x-opencode-session"]; got != "ses_client" {
		t.Fatalf("client session overwritten: %v", got)
	}

	// The actual session id is the next fallback.
	pkt = packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyHTTPMeta, map[string]any{
		"headers": map[string]any{"x-opencode-session-id": "ses_id"},
	})
	ensureOpenCodeSession(pkt, req, &auth.Principal{ID: "k1"}, true)
	headers := ensureMeta(pkt)
	if headers["x-opencode-session"] != "ses_id" || headers["x-session-affinity"] != "ses_id" {
		t.Fatalf("session id fallback = %v", headers)
	}

	// A generic X-Session-ID is also used.
	pkt = packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyHTTPMeta, map[string]any{"session_id": "generic"})
	ensureOpenCodeSession(pkt, req, &auth.Principal{ID: "k1"}, true)
	if got := ensureMeta(pkt)["x-opencode-session"]; got != "generic" {
		t.Fatalf("generic session fallback = %v", got)
	}

	// Otherwise a stable ses_... is generated per key/client/project.
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	request.Header.Set("User-Agent", "opencode/2")
	makePacket := func(project string) packet.Packet {
		p := packet.NewReq(map[string]any{"model": "m"})
		p.Set(packet.KeyHTTPMeta, map[string]any{
			"headers": map[string]any{"x-opencode-project": project},
		})
		return p
	}
	a := makePacket("p1")
	ensureOpenCodeSession(a, request, &auth.Principal{ID: "k1"}, true)
	first := ensureMeta(a)["x-opencode-session"].(string)
	if !strings.HasPrefix(first, "ses_") || len(first) != len("ses_")+64 {
		t.Fatalf("generated session = %q", first)
	}
	b := makePacket("p1")
	ensureOpenCodeSession(b, request, &auth.Principal{ID: "k1"}, true)
	if got := ensureMeta(b)["x-opencode-session"]; got != first {
		t.Fatalf("generated session is not stable: %v vs %v", got, first)
	}
	c := makePacket("p1")
	ensureOpenCodeSession(c, request, &auth.Principal{ID: "k2"}, true)
	if got := ensureMeta(c)["x-opencode-session"]; got == first {
		t.Fatal("different keys should get different sessions")
	}
}
