package handler

import (
	"testing"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// Attribution is by the serving backend (the request model after the alias/pool
// rewrite).
func TestRatelimitReportsRotation(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pooled", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)

	serve := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m") // simulate the alias rule rewrite
		}
		return pkt.FailStatus(packet.ErrUpstream, 429, 30, packet.ClassRateLimit, "boom")
	})
	pkt := packet.NewReq(map[string]any{"model": "pooled"})
	h.Handle(pkt, serve)

	if got, _ := ctrl.Active("pooled"); got != "b/m" {
		t.Fatalf("expected rotation to b/m, got %q", got)
	}
}

func TestRatelimitReportsQuotaAsDead(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pooled", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)

	serve := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m")
		}
		return pkt.FailStatus(packet.ErrUpstream, 402, 0, packet.ClassQuota, "insufficient balance")
	})
	h.Handle(packet.NewReq(map[string]any{"model": "pooled"}), serve)

	if got, _ := ctrl.Active("pooled"); got != "b/m" {
		t.Fatalf("expected dead a/m to be skipped, got %q", got)
	}
	// a successful request served by a/m revives it (dead clears on success)
	serveOK := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m")
		}
		return pkt
	})
	h.Handle(packet.NewReq(map[string]any{"model": "pooled"}), serveOK)
	status, _ := ctrl.Status("pooled")
	if status.Health["a/m"].Health != string(pool.HealthNormal) {
		t.Fatalf("success should revive a/m, got %+v", status.Health["a/m"])
	}
}

func TestRatelimitIgnoresNonPoolAndInvalid(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pooled", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)

	// request served by a backend not owned by any pool: no report
	servePlain := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		return pkt.FailStatus(packet.ErrUpstream, 429, 30, packet.ClassRateLimit, "boom")
	})
	h.Handle(packet.NewReq(map[string]any{"model": "plain"}), servePlain)
	if got, _ := ctrl.Active("pooled"); got != "a/m" {
		t.Fatalf("non-pool backend must not rotate, got %q", got)
	}

	// invalid class is not a health signal
	serveInvalid := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m")
		}
		return pkt.FailStatus(packet.ErrUpstream, 400, 0, packet.ClassInvalid, "bad request")
	})
	h.Handle(packet.NewReq(map[string]any{"model": "pooled"}), serveInvalid)
	if got, _ := ctrl.Active("pooled"); got != "a/m" {
		t.Fatalf("invalid must not rotate, got %q", got)
	}
}
