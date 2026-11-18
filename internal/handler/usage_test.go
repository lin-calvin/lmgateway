package handler

import (
	"testing"

	"lmgateway/internal/hook"
	"lmgateway/internal/packet"
	"lmgateway/internal/ratelimit"
	"lmgateway/internal/usage"
)

func TestUsageCollectorEmitsRateEventOnError(t *testing.T) {
	rateHooks := hook.New[ratelimit.Event]()
	var got ratelimit.Event
	rateHooks.Register(func(ev ratelimit.Event) { got = ev })

	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyProvider, "opencode_go")
	pkt.Set(packet.KeyUpstreamHeaders, map[string]any{
		"ratelimit-limit":     "10",
		"ratelimit-remaining": "2",
	})

	collector := UsageCollector{RateHooks: rateHooks}
	collector.Handle(pkt, noopServe)

	if got.Provider != "opencode_go" || got.Headers["ratelimit-limit"] != "10" {
		t.Fatalf("rate event = %+v", got)
	}
}

func TestUsageCollectorEmitsBoth(t *testing.T) {
	rateHooks := hook.New[ratelimit.Event]()
	usageHooks := hook.New[usage.Usage]()
	rateCount, usageCount := 0, 0
	rateHooks.Register(func(ratelimit.Event) { rateCount++ })
	usageHooks.Register(func(usage.Usage) { usageCount++ })

	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyProvider, "p")
	pkt.Set(packet.KeyUpstreamHeaders, map[string]any{"ratelimit-limit": "1", "ratelimit-remaining": "1"})
	pkt.Set("resp", map[string]any{
		"model": "m",
		"usage": map[string]any{"prompt_tokens": 1.0, "completion_tokens": 1.0},
	})
	pkt.SetPhase(packet.PhaseResp)

	collector := UsageCollector{Hooks: usageHooks, RateHooks: rateHooks}
	collector.Handle(pkt, noopServe)

	if rateCount != 1 || usageCount != 1 {
		t.Fatalf("rate=%d usage=%d", rateCount, usageCount)
	}
}
