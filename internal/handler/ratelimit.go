package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// Ratelimit is a passive, pool-aware reporter. It wraps the request (wired
// between observe and http) and reports the outcome to the pool control plane.
// It never retries and never changes the response.
//
// Attribution is by the *serving* backend (the request model after pool/alias
// rewriting), so a request downgraded to a fallback cools the fallback's owner.
type Ratelimit struct {
	Ctrl *pool.Controller
}

func NewRatelimit(ctrl *pool.Controller) Ratelimit {
	return Ratelimit{Ctrl: ctrl}
}

func (h Ratelimit) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	out := serve(pkt)
	backend := requestModel(out)
	if backend == "" {
		return out
	}
	if class, ok := out.ErrorClass(); ok {
		if packet.IsHealthClass(class) {
			retryAfter, _ := out.ErrorRetryAfter()
			h.Ctrl.ReportBackend(backend, class, retryAfter)
		}
		return out
	}
	// success: optimistically revive a transient ratelimit (dead needs a probe)
	h.Ctrl.ClearRateLimit(backend)
	return out
}

func requestModel(pkt packet.Packet) string {
	doc, ok := pkt.Request()
	if !ok {
		return ""
	}
	value, ok := doc.Get("model")
	if !ok {
		return ""
	}
	model, _ := value.(string)
	return model
}
