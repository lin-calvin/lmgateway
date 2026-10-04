package handler

import (
	"testing"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// The reporter snapshots the original model (pool alias) on entry and reads the
// rewritten model (backend id) after the pipeline returns.
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

func TestRatelimitIgnoresNonPoolAndOtherClasses(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pooled", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)

	// non-pool request: no report
	serve := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m")
		}
		return pkt.FailStatus(packet.ErrUpstream, 429, 30, packet.ClassRateLimit, "boom")
	})
	h.Handle(packet.NewReq(map[string]any{"model": "plain"}), serve)
	if got, _ := ctrl.Active("pooled"); got != "a/m" {
		t.Fatalf("non-pool request must not rotate, got %q", got)
	}

	// non-rate-limit class: no report
	serve500 := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		if doc, ok := pkt.Request(); ok {
			_ = doc.Set("model", "a/m")
		}
		return pkt.FailStatus(packet.ErrUpstream, 500, 0, packet.ClassUpstream, "boom")
	})
	h.Handle(packet.NewReq(map[string]any{"model": "pooled"}), serve500)
	if got, _ := ctrl.Active("pooled"); got != "a/m" {
		t.Fatalf("non-rate-limit must not rotate, got %q", got)
	}
}
