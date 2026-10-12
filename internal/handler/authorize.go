package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
)

// Authorizer is the single authorization point for the data plane. It runs at
// ingress, on the client's requested model name, before any routing rewrite.
//
// The public namespace decides what may be addressed and by whom; the routing
// graph (alias targets, pool backends, provider bindings) is internal and carries
// no ACL. Resolution is injected by the config compiler to keep the handler free
// of a config dependency.
type Authorizer struct {
	resolve func(model string) (allowed, denied []string, ok bool)
	known   func(model string) bool
}

func NewAuthorizer(resolve func(model string) (allowed, denied []string, ok bool), known func(model string) bool) Authorizer {
	return Authorizer{resolve: resolve, known: known}
}

func (h Authorizer) Handle(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
	model := requestModel(pkt)
	groups := pkt.PrincipalGroups()
	if hasGroup(groups, "*") { // root (master key) or open mode
		return serve(pkt)
	}
	if allowed, denied, ok := h.resolve(model); ok {
		if groupsAllow(groups, allowed, denied) {
			return serve(pkt)
		}
		return denyModel(pkt, model)
	}
	// Not a public name. If it is a known routing target (an internal backend),
	// it exists but is not addressable -> 403. Otherwise let routing return 404.
	if h.known != nil && h.known(model) {
		return denyModel(pkt, model)
	}
	return serve(pkt)
}

func denyModel(pkt packet.Packet, model string) packet.Packet {
	return pkt.SetErrorReason("model_not_permitted").SetErrorScope(model).
		FailStatus(packet.ErrPolicy, 403, 0, "", "model "+model+" is not permitted for this principal")
}

func hasGroup(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}

// groupsAllow mirrors the namespace ACL semantics: root "*" passes; deny wins;
// empty allow list = unrestricted.
func groupsAllow(groups, allowed, denied []string) bool {
	if hasGroup(groups, "*") {
		return true
	}
	for _, d := range denied {
		if hasGroup(groups, d) {
			return false
		}
	}
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if hasGroup(groups, a) {
			return true
		}
	}
	return false
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
