package clientmeta

import (
	"net/http"
	"testing"

	"lmgateway/internal/packet"
)

func TestApply(t *testing.T) {
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyHTTPMeta, map[string]any{
		"headers": map[string]any{
			"x-opencode-session": "ses_test",
			"x-opencode-project": "proj",
			"x-session-affinity": "aff",
		},
	})
	req, err := http.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	Apply(req, pkt)
	if got := req.Header.Get("X-Opencode-Session"); got != "ses_test" {
		t.Fatalf("x-opencode-session = %q", got)
	}
	if got := req.Header.Get("X-Opencode-Project"); got != "proj" {
		t.Fatalf("x-opencode-project = %q", got)
	}
	if got := req.Header.Get("X-Session-Affinity"); got != "aff" {
		t.Fatalf("x-session-affinity = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("authorization must not be forwarded: %q", got)
	}
}

func TestApplyWithoutMetadata(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.test", nil)
	Apply(req, packet.NewReq(map[string]any{"model": "m"}))
	if len(req.Header) != 0 {
		t.Fatalf("headers changed without metadata: %v", req.Header)
	}
}
