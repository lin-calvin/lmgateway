package codex

import (
	"lmgateway/internal/planstate"
)

// RateLimitSource consumes the typed snapshot produced by the Codex handler or
// parses its x-codex-* headers when only headers are available.
func RateLimitSource() planstate.Source {
	return planstate.Source{
		Sink: func(ev planstate.Event) *planstate.Snapshot {
			if ev.Snapshot != nil {
				return ev.Snapshot
			}
			snaps := planstate.ParseCodexHeaders(ev.Headers)
			if len(snaps) == 0 {
				return nil
			}
			return &snaps[0]
		},
	}
}
