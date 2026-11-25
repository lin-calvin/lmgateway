package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/hook"
	"lmgateway/internal/packet"
	"lmgateway/internal/planstate"
	"lmgateway/internal/usage"
)

// UsageCollector is the dispatcher handler at the "usage" position. It normalizes
// the response into a usage.Usage and fans it out to registered subscribers
// (the spend sink by default); it holds no policy itself.
type UsageCollector struct {
	Hooks     *hook.Registry[usage.Usage]
	RateHooks *hook.Registry[planstate.Event]
}

// Handle extracts the usage and rate-limit metadata and emits them, then
// continues the route. The rate-limit event is emitted even for 429/402
// responses, where there is no Usage.
func (c UsageCollector) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	if c.RateHooks != nil {
		if ev, ok := planstate.EventFromPacket(pkt); ok {
			c.RateHooks.Emit(ev)
		}
	}
	if c.Hooks != nil {
		if u, ok := usage.Extract(pkt); ok {
			c.Hooks.Emit(u)
		}
	}
	return serve(pkt)
}
