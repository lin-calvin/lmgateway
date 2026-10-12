package handler

import (
	"testing"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
)

func accessServe(pkt packet.Packet, _ ...string) packet.Packet { return pkt }

func principal(groups ...string) map[string]any {
	return map[string]any{"id": "k1", "kind": "apikey", "groups": groups}
}

func TestAccessCheck(t *testing.T) {
	next := func(pkt packet.Packet, serve dispatch.Serve) packet.Packet { return pkt }
	check := AccessCheck(next, []string{"teamA"}, []string{"banned"})

	run := func(groups []string) packet.Packet {
		pkt := packet.NewReq(map[string]any{"model": "m"})
		pkt.Set(packet.KeyPrincipal, principal(groups...))
		return check(pkt, accessServe)
	}

	if _, isErr := run([]string{"teamA"}).Error(); isErr {
		t.Fatal("teamA should be allowed")
	}
	out := run([]string{"teamB"})
	if _, isErr := out.Error(); !isErr {
		t.Fatal("teamB should be denied")
	}
	if status, _ := out.ErrorStatus(); status != 403 {
		t.Fatalf("expected 403, got %d", status)
	}
	if kind, _ := out.ErrorKind(); kind != packet.ErrPolicy {
		t.Fatalf("expected policy kind, got %q", kind)
	}
	// denied wins even if allowed
	if _, isErr := run([]string{"teamA", "banned"}).Error(); !isErr {
		t.Fatal("banned must win over allowed")
	}
	// root ("*") bypasses
	if _, isErr := run([]string{"*"}).Error(); isErr {
		t.Fatal("root should pass")
	}
	// no principal (internal probe) passes
	pkt := packet.NewReq(map[string]any{"model": "m"})
	if _, isErr := check(pkt, accessServe).Error(); isErr {
		t.Fatal("no principal should pass")
	}
}

func TestAccessCheckEmptyAllowUnrestricted(t *testing.T) {
	next := func(pkt packet.Packet, serve dispatch.Serve) packet.Packet { return pkt }
	check := AccessCheck(next, nil, nil)
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyPrincipal, principal("anyone"))
	if _, isErr := check(pkt, accessServe).Error(); isErr {
		t.Fatal("empty allow list should be unrestricted")
	}
}
