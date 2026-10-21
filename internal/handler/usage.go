package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/hook"
	"lmgateway/internal/packet"
	"lmgateway/internal/usage"
)

// UsageCollector is the dispatcher handler at the "usage" position. It normalizes
// the response into a usage.Usage and fans it out to registered subscribers
// (the spend sink by default); it holds no policy itself.
type UsageCollector struct {
	Hooks *hook.Registry[usage.Usage]
}

// Handle extracts the usage, emits it to subscribers, then continues the route.
func (c UsageCollector) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	if c.Hooks != nil {
		if u, ok := usage.Extract(pkt); ok {
			c.Hooks.Emit(u)
		}
	}
	return serve(pkt)
}
