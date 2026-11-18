package codex

import (
	"lmgateway/internal/codingplan"
	"lmgateway/internal/ratelimit"
)

// RateLimitSource consumes the typed snapshot produced by the Codex handler or
// parses its x-codex-* headers when only headers are available.
func RateLimitSource() codingplan.Source {
	return codingplan.Source{
		Sink: func(ev ratelimit.Event) *ratelimit.Snapshot {
			if ev.Snapshot != nil {
				return ev.Snapshot
			}
			snaps := ratelimit.ParseCodexHeaders(ev.Headers)
			if len(snaps) == 0 {
				return nil
			}
			return &snaps[0]
		},
	}
}
