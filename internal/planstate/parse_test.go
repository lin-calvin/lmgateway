package planstate

import (
	"io"
	"strings"
	"testing"

	"lmgateway/internal/packet"
)

func TestParseGeneric(t *testing.T) {
	snap := ParseGeneric(map[string]string{
		"ratelimit-limit":       "100",
		"ratelimit-remaining":   "75",
		"ratelimit-reset-after": "3600",
	})
	if snap == nil || len(snap.Windows) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "default" || w.Limit != 100 || w.Remaining != 75 || w.UsedPercent != 25 || w.ResetAfterSec != 3600 {
		t.Fatalf("window = %+v", w)
	}
}

func TestParseGenericSuffixed(t *testing.T) {
	snap := ParseGeneric(map[string]string{
		"x-ratelimit-limit-requests":     "200",
		"x-ratelimit-remaining-requests": "50",
		"x-ratelimit-reset-requests":     "1700000000",
	})
	if snap == nil || len(snap.Windows) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "requests" || w.UsedPercent != 75 || w.ResetAt.IsZero() {
		t.Fatalf("window = %+v", w)
	}
}

func TestParseCodexHeaders(t *testing.T) {
	snaps := ParseCodexHeaders(map[string]string{
		"x-codex-primary-used-percent":     "42",
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-reset-at":         "1700000000",
		"x-codex-secondary-used-percent":   "15",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-credits-has-credits":      "true",
		"x-codex-credits-unlimited":        "false",
		"x-codex-credits-balance":          "12.5",
		"x-codex-plan-type":                "plus",
	})
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	snap := snaps[0]
	if snap.LimitID != "codex" || snap.PlanType != "plus" || len(snap.Windows) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Windows[0].Name != "primary" || snap.Windows[0].UsedPercent != 42 || snap.Windows[0].WindowMinutes != 300 {
		t.Fatalf("primary = %+v", snap.Windows[0])
	}
	if snap.Credits == nil || snap.Credits.Balance != "12.5" || !snap.Credits.HasCredits {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestParseCodexEvent(t *testing.T) {
	snap, ok := ParseCodexEvent([]byte(`{
		"type":"codex.rate_limits",
		"plan_type":"pro",
		"rate_limits":{
			"primary":{"used_percent":10,"window_minutes":300,"reset_at":1700000000},
			"secondary":{"used_percent":20,"window_minutes":10080,"reset_at":1700003600}
		},
		"credits":{"has_credits":false,"unlimited":false}
	}`))
	if !ok || snap == nil || snap.PlanType != "pro" || len(snap.Windows) != 2 {
		t.Fatalf("snapshot = %+v ok=%v", snap, ok)
	}
}

func TestEventFromPacket(t *testing.T) {
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set("provider", "opencode_go")
	pkt.Set(packet.KeyUpstreamHeaders, map[string]any{"ratelimit-limit": "10", "ratelimit-remaining": "5"})
	ev, ok := EventFromPacket(pkt)
	if !ok || ev.Provider != "opencode_go" || ev.Headers["ratelimit-limit"] != "10" {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}

	pkt2 := packet.NewReq(map[string]any{"model": "m"})
	pkt2.Set("provider", "chatgpt_codex")
	pkt2.Set(packet.KeyPlanSnapshot, Snapshot{Provider: "chatgpt_codex", Windows: []Window{{Name: "primary", UsedPercent: 1}}})
	ev2, ok := EventFromPacket(pkt2)
	if !ok || ev2.Snapshot == nil || ev2.Snapshot.Windows[0].UsedPercent != 1 {
		t.Fatalf("event2 = %+v ok=%v", ev2, ok)
	}
}

func TestParseGenericNoHeaders(t *testing.T) {
	if snap := ParseGeneric(map[string]string{"content-type": "application/json"}); snap != nil {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestCaptureSSE(t *testing.T) {
	capture := NewCapture()
	body := CaptureSSE(io.NopCloser(strings.NewReader(
		"event: codex.rate_limits\n"+
			"data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"plus\",\"rate_limits\":{\"secondary\":{\"used_percent\":30,\"window_minutes\":10080}}}\n\n",
	)), capture)
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	snap := capture.Snapshot()
	if snap == nil || snap.PlanType != "plus" || len(snap.Windows) != 1 || snap.Windows[0].UsedPercent != 30 {
		t.Fatalf("snapshot = %+v", snap)
	}

	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set("provider", "codex")
	pkt.Set(packet.KeyPlanCapture, capture)
	ev, ok := EventFromPacket(pkt)
	if !ok || ev.Snapshot == nil || ev.Snapshot.PlanType != "plus" {
		t.Fatalf("event = %+v ok=%v", ev, ok)
	}
}

func TestParseOpenCodeUsage(t *testing.T) {
	snap, ok := ParseOpenCodeUsage([]byte(`{"usage":{
		"rolling":{"status":"ok","percent":0,"resetsAt":"2026-11-18T19:40:12.000Z"},
		"weekly":{"status":"ok","percent":26,"resetsAt":"2026-11-23T00:00:00.000Z"},
		"monthly":{"status":"rate-limited","percent":100,"resetsAt":"2026-12-04T13:58:55.000Z"}
	}}`))
	if !ok || snap == nil || snap.LimitID != "opencode" || len(snap.Windows) != 3 {
		t.Fatalf("snapshot = %+v ok=%v", snap, ok)
	}
	if snap.Windows[0].Name != "rolling" || snap.Windows[0].WindowMinutes != 300 || snap.Windows[0].RemainingPercent != 100 {
		t.Fatalf("rolling = %+v", snap.Windows[0])
	}
	if snap.Windows[1].UsedPercent != 26 || snap.Windows[1].RemainingPercent != 74 {
		t.Fatalf("weekly = %+v", snap.Windows[1])
	}
	if snap.Windows[2].Status != "rate-limited" || snap.Windows[2].RemainingPercent != 0 {
		t.Fatalf("monthly = %+v", snap.Windows[2])
	}
}
