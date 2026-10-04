package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
)

// Ratelimit is a passive, pool-aware reporter. It wraps the request (wired
// between observe and http) and, once the pipeline returns, reports a rate-limit
// failure to the pool control plane. It never retries and never changes the
// response; the data plane contains no pool logic beyond this reporting hook.
//
// Pools are compiled to ordinary alias rules, so the request model is rewritten
// (alias → backend id) before the provider runs. The reporter snapshots the
// original model on entry and reads the rewritten model on the way out to
// identify both the pool and the failing backend.
type Ratelimit struct {
	Ctrl *pool.Controller
}

func NewRatelimit(ctrl *pool.Controller) Ratelimit {
	return Ratelimit{Ctrl: ctrl}
}

func (h Ratelimit) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	original := requestModel(pkt)
	out := serve(pkt)
	class, ok := out.ErrorClass()
	if !ok || class != packet.ClassRateLimit {
		return out
	}
	backend := requestModel(out)
	if original == "" || backend == "" || backend == original {
		return out
	}
	retryAfter, _ := out.ErrorRetryAfter()
	h.Ctrl.Report(original, backend, class, retryAfter)
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
