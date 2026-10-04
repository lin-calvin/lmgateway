package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// Pool is the data-plane node for a backend pool: on entry it rewrites the
// request model to the pool's active backend and re-enters routing so the
// backend id resolves through the normal model/prefix edges. Selection is sticky
// (no round-robin) to preserve provider-side prefix/KV cache.
type Pool struct {
	Model string
	Ctrl  *pool.Controller
}

func (h Pool) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	backend, ok := h.Ctrl.Active(h.Model)
	if !ok {
		return serve(pkt.FailStatus(packet.ErrUpstream, 429, h.Ctrl.RetryAfter(h.Model),
			packet.ClassRateLimit, "pool "+h.Model+": all backends rate limited"))
	}
	pkt.Set("pool", h.Model)
	pkt.Set("pool_backend", backend)
	if err := pkt.SetPath("model", backend); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, "pool rewrite failed: "+err.Error()))
	}
	return serve(pkt, "http")
}
