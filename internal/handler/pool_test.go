package handler

import (
	"strings"
	"testing"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

func TestPoolHandlerRewritesAndReenters(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pool-m", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := Pool{Model: "pool-m", Ctrl: ctrl}

	var gotSource, gotModel string
	serve := dispatch.Serve(func(pkt packet.Packet, sources ...string) packet.Packet {
		gotSource = strings.Join(sources, ",")
		doc, _ := pkt.Request()
		if value, ok := doc.Get("model"); ok {
			gotModel, _ = value.(string)
		}
		return pkt
	})

	pkt := packet.NewReq(map[string]any{"model": "pool-m", "messages": []any{}})
	h.Handle(pkt, serve)

	if gotModel != "a/m" {
		t.Fatalf("expected model rewritten to a/m, got %q", gotModel)
	}
	if gotSource != "http" {
		t.Fatalf("expected re-entry at http, got %q", gotSource)
	}
	if name, _ := pkt.Str("pool"); name != "pool-m" {
		t.Fatalf("pool marker missing: %q", name)
	}
}

func TestPoolHandlerAllLimited(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pool-m", Backend: []string{"a/m"}, CooldownSec: 60})
	ctrl.Report("pool-m", "a/m", "rate_limit", 30)
	h := Pool{Model: "pool-m", Ctrl: ctrl}

	out := h.Handle(packet.NewReq(map[string]any{"model": "pool-m"}), func(pkt packet.Packet, _ ...string) packet.Packet {
		return pkt
	})
	class, _ := out.ErrorClass()
	if class != packet.ClassRateLimit {
		t.Fatalf("expected rate_limit class, got %q", class)
	}
	if status, _ := out.ErrorStatus(); status != 429 {
		t.Fatalf("expected 429, got %d", status)
	}
}

func TestRatelimitReports(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pool-m", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)

	serve := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		pkt.Set(packet.KeyProvider, "a")
		return pkt.FailStatus(packet.ErrUpstream, 429, 30, packet.ClassRateLimit, "boom")
	})
	pkt := packet.NewReq(map[string]any{"model": "pool-m"})
	pkt.Set("pool", "pool-m")
	pkt.Set("pool_backend", "a/m")
	h.Handle(pkt, serve)

	if got, _ := ctrl.Active("pool-m"); got != "b/m" {
		t.Fatalf("expected rotation to b/m, got %q", got)
	}
}

func TestRatelimitIgnoresNonPool(t *testing.T) {
	ctrl := pool.NewController()
	ctrl.Register(pool.Config{Model: "pool-m", Backend: []string{"a/m", "b/m"}, CooldownSec: 60})
	h := NewRatelimit(ctrl)
	serve := dispatch.Serve(func(pkt packet.Packet, _ ...string) packet.Packet {
		pkt.Set(packet.KeyProvider, "a")
		return pkt.FailStatus(packet.ErrUpstream, 429, 30, packet.ClassRateLimit, "boom")
	})
	pkt := packet.NewReq(map[string]any{"model": "plain"})
	h.Handle(pkt, serve)
	if got, _ := ctrl.Active("pool-m"); got != "a/m" {
		t.Fatalf("non-pool request must not rotate, got %q", got)
	}
}
