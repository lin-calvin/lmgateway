// Package codingplan defines provider capabilities that live outside the data
// path: periodic usage sources and per-response rate-limit sinks.
package codingplan

import (
	"context"

	"lmgateway/internal/ratelimit"
)

// Sink handles one response's rate-limit event. Returning nil means the event
// did not carry an update for this provider.
type Sink func(ratelimit.Event) *ratelimit.Snapshot

// Timer fetches an account-level usage snapshot. It runs on a control-plane
// ticker, never on the request path.
type Timer func(context.Context) (*ratelimit.Snapshot, error)

// Source is the optional plan capability set for one provider.
type Source struct {
	Sink  Sink
	Timer Timer
}
