package openai

import (
	"lmgateway/internal/codingplan"
	"lmgateway/internal/ratelimit"
)

// RateLimitSource parses generic ratelimit-* / x-ratelimit-* response headers.
func RateLimitSource() codingplan.Source {
	return codingplan.Source{
		Sink: func(ev ratelimit.Event) *ratelimit.Snapshot {
			if ev.Snapshot != nil {
				return ev.Snapshot
			}
			return ratelimit.ParseGeneric(ev.Headers)
		},
	}
}
