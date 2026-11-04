// Package clientmeta copies allowlisted client metadata headers onto an
// upstream request. It carries OpenCode session/affinity headers (x-opencode-*,
// x-session-affinity, x-session-id) without exposing auth or arbitrary client
// headers to the provider.
package clientmeta

import (
	"net/http"

	"lmgateway/internal/packet"
)

// Apply sets the headers collected from the inbound request's http_meta onto
// req. A missing metadata map is a no-op.
func Apply(req *http.Request, pkt packet.Packet) {
	meta, ok := pkt.Map(packet.KeyHTTPMeta)
	if !ok {
		return
	}
	headers, ok := meta["headers"].(map[string]any)
	if !ok {
		return
	}
	for name, value := range headers {
		s, ok := value.(string)
		if !ok || s == "" {
			continue
		}
		req.Header.Set(name, s)
	}
}
