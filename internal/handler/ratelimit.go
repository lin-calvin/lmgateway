package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// Ratelimit is a passive response-stage reporter wired between each provider and
// usage. It inspects the final packet for a retryable upstream class and lets the
// pool control plane rotate. It never retries and never changes the response.
type Ratelimit struct {
	Ctrl     *pool.Controller
	RotateOn map[string]bool
}

func NewRatelimit(ctrl *pool.Controller) Ratelimit {
	return Ratelimit{
		Ctrl: ctrl,
		RotateOn: map[string]bool{
			packet.ClassRateLimit: true,
		},
	}
}

func (h Ratelimit) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	out := serve(pkt)
	class, ok := out.ErrorClass()
	if !ok || !h.RotateOn[class] {
		return out
	}
	model, ok := out.Str("pool")
	if !ok || model == "" {
		return out
	}
	// Prefer the pool member id set by the pool handler; fall back to the
	// provider name for pools whose members are providers.
	backend, ok := out.Str("pool_backend")
	if !ok || backend == "" {
		backend, _ = out.Str(packet.KeyProvider)
	}
	retryAfter, _ := out.ErrorRetryAfter()
	h.Ctrl.Report(model, backend, class, retryAfter)
	return out
}
