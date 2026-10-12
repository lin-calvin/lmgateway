package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
)

// AccessCheck wraps a downstream handler (typically a provider) with a group-based
// resource ACL. It runs after model resolution (alias/pool rewrite) and before the
// provider, so a caller cannot bypass the ACL via an alias.
//
// Semantics: denied groups always win; an empty allowed list means "unrestricted";
// otherwise the caller's groups must intersect allowed. The root principal carries
// the "*" group and passes everything. Internal callers with no resolved principal
// (e.g. pool probes) pass.
func AccessCheck(next dispatch.HandlerFunc, allowed, denied []string) dispatch.HandlerFunc {
	return func(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
		groups := pkt.PrincipalGroups()
		if _, hasPrincipal := pkt.PrincipalOf(); !hasPrincipal {
			return next(pkt, serve)
		}
		if hasGroup(groups, "*") {
			return next(pkt, serve)
		}
		for _, g := range denied {
			if hasGroup(groups, g) {
				return denyAccess(pkt, g)
			}
		}
		if len(allowed) > 0 && !intersects(groups, allowed) {
			return denyAccess(pkt, "")
		}
		return next(pkt, serve)
	}
}

func denyAccess(pkt packet.Packet, scope string) packet.Packet {
	return pkt.SetErrorReason("group_denied").SetErrorScope(scope).
		FailStatus(packet.ErrPolicy, 403, 0, "", "model not permitted for this principal")
}

func hasGroup(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}

func intersects(groups, allowed []string) bool {
	for _, a := range allowed {
		if hasGroup(groups, a) {
			return true
		}
	}
	return false
}
