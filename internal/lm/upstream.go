package lm

import "lmgateway/internal/keytag"

// UpstreamMap returns a shallow copy of the document's raw map with
// gateway-internal fields removed, so they are never sent to a provider.
func UpstreamMap(doc LMDocument) (map[string]any, bool) {
	raw, ok := Map(doc)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		switch k {
		case keytag.TagField, keytag.HintField, keytag.HintsField:
			continue
		}
		out[k] = v
	}
	return out, true
}
