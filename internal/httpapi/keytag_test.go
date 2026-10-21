package httpapi

import (
	"context"
	"testing"

	"lmgateway/internal/auth"
	"lmgateway/internal/keytag"
	"lmgateway/internal/packet"
)

func documentTag(t *testing.T, pkt packet.Packet) (string, bool) {
	t.Helper()
	req, ok := pkt.Request()
	if !ok {
		t.Fatal("packet has no request document")
	}
	raw, ok := req.Get(keytag.TagField)
	if !ok {
		return "", false
	}
	tag, _ := raw.(string)
	return tag, true
}

func documentHints(t *testing.T, pkt packet.Packet) map[string]any {
	t.Helper()
	req, ok := pkt.Request()
	if !ok {
		t.Fatal("packet has no request document")
	}
	raw, ok := req.Get(keytag.HintsField)
	if !ok {
		return nil
	}
	hints, _ := raw.(map[string]any)
	return hints
}

func applySuffix(pkt packet.Packet, suffix string) {
	applyRouteMeta(pkt, auth.WithKeySuffix(context.Background(), suffix))
}

func TestApplyRouteMeta(t *testing.T) {
	// Attribution tag and policy hint come from the same suffix.
	pkt := packet.NewReq(map[string]any{"model": "m"})
	applySuffix(pkt, "cli&pool_policy=price_first")
	if tag, _ := documentTag(t, pkt); tag != "cli" {
		t.Fatalf("tag = %q", tag)
	}
	hints := documentHints(t, pkt)
	if hints["pool_policy"] != "price_first" || len(hints) != 1 {
		t.Fatalf("hints = %v", hints)
	}

	// Hints-only suffix (no attribution tag) is allowed.
	pkt = packet.NewReq(map[string]any{"model": "m"})
	applySuffix(pkt, "pool_policy=speed_first")
	if tag, ok := documentTag(t, pkt); ok {
		t.Fatalf("unexpected tag %q", tag)
	}
	if hints := documentHints(t, pkt); hints["pool_policy"] != "speed_first" {
		t.Fatalf("hints = %v", hints)
	}

	// A valid body key_tag wins over the suffix tag.
	pkt = packet.NewReq(map[string]any{"model": "m", keytag.TagField: "body"})
	applySuffix(pkt, "cli&pool_policy=price_first")
	if tag, _ := documentTag(t, pkt); tag != "body" {
		t.Fatalf("body tag should win, got %q", tag)
	}

	// A body route_hint wins over the suffix hints.
	pkt = packet.NewReq(map[string]any{"model": "m", keytag.HintField: "pool_policy=price_first"})
	applySuffix(pkt, "cli&pool_policy=speed_first")
	if hints := documentHints(t, pkt); hints["pool_policy"] != "price_first" {
		t.Fatalf("body route_hint should win: %v", hints)
	}

	// An invalid body tag falls back to the suffix tag.
	pkt = packet.NewReq(map[string]any{"model": "m", keytag.TagField: "a b"})
	applySuffix(pkt, "cli")
	if tag, _ := documentTag(t, pkt); tag != "cli" {
		t.Fatalf("invalid body tag should fall back: %q", tag)
	}

	// Without a suffix, an invalid body tag is removed.
	pkt = packet.NewReq(map[string]any{"model": "m", keytag.TagField: "a b"})
	applyRouteMeta(pkt, context.Background())
	if tag, ok := documentTag(t, pkt); ok {
		t.Fatalf("invalid body tag should be removed, got %q", tag)
	}
}
