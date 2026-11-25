// Package ratelimit holds provider-neutral subscription rate-limit snapshots.
// Provider adapters either parse their own headers (Codex) or put raw headers
// into the packet for the plan manager to parse.
package planstate

import (
	"context"
	"net/http"
	"strings"
	"time"

	"lmgateway/internal/packet"
)

// Window is one rate-limit window (5h, weekly, primary, secondary, ...).
type Window struct {
	Name             string    `json:"name"`
	Status           string    `json:"status,omitempty"`
	WindowMinutes    int       `json:"window_minutes,omitempty"`
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent,omitempty"`
	Limit            float64   `json:"limit,omitempty"`
	Remaining        float64   `json:"remaining,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	ResetAfterSec    int       `json:"reset_after_sec,omitempty"`
}

// Credits is the extra-usage credit balance when the provider reports it.
type Credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

// Snapshot is the latest observed rate-limit state for one provider/limit.
type Snapshot struct {
	Provider    string            `json:"provider"`
	LimitID     string            `json:"limit_id,omitempty"`
	LimitName   string            `json:"limit_name,omitempty"`
	PlanType    string            `json:"plan_type,omitempty"`
	ActiveLimit string            `json:"active_limit,omitempty"`
	Windows     []Window          `json:"windows,omitempty"`
	Credits     *Credits          `json:"credits,omitempty"`
	Raw         map[string]string `json:"raw,omitempty"`
	ObservedAt  time.Time         `json:"observed_at"`
}

// Sink handles one response's rate-limit event. Returning nil means the event
// did not carry an update for this provider.
type Sink func(Event) *Snapshot

// Timer fetches an account-level usage snapshot. It runs on a control-plane
// ticker, never on the request path.
type Timer func(context.Context) (*Snapshot, error)

// Source is the optional plan capability set for one provider.
type Source struct {
	Sink  Sink
	Timer Timer
}

// SnapshotCapture is implemented by stream captures (Codex SSE).
type SnapshotCapture interface {
	Snapshot() *Snapshot
}

// Event is emitted by the usage collector after every provider response. It is
// produced even for 429/402 responses, where there is no Usage.
type Event struct {
	Provider string
	Model    string
	Snapshot *Snapshot
	Headers  map[string]string
	At       time.Time
}

// HeaderMap converts http.Header into the map[string]any packet shape with
// lowercase names.
func HeaderMap(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		out[strings.ToLower(name)] = values[0]
	}
	return out
}

// StringHeaders normalizes a packet header map into lowercase string values.
func StringHeaders(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for name, value := range in {
		s, ok := value.(string)
		if !ok || s == "" {
			continue
		}
		out[strings.ToLower(name)] = s
	}
	return out
}

// EventFromPacket builds an Event from a packet carrying either a parsed
// snapshot (Codex) or raw upstream headers (OpenAI-compatible providers).
func EventFromPacket(pkt packet.Packet) (Event, bool) {
	provider, _ := pkt.Str(packet.KeyProvider)
	model := ""
	if resp, ok := pkt.Map(packet.KeyResp); ok {
		model, _ = resp["model"].(string)
	}
	if model == "" {
		model, _ = pkt.Str("model")
	}

	var snap *Snapshot
	if capture, ok := pkt[packet.KeyPlanCapture].(SnapshotCapture); ok && capture != nil {
		snap = capture.Snapshot()
	}
	switch v := pkt[packet.KeyPlanSnapshot].(type) {
	case Snapshot:
		if snap == nil {
			s := v
			snap = &s
		}
	case *Snapshot:
		if snap == nil {
			snap = v
		}
	case []Snapshot:
		if snap == nil && len(v) > 0 {
			s := v[0]
			snap = &s
		}
	}

	var headers map[string]string
	if raw, ok := pkt.Map(packet.KeyUpstreamHeaders); ok {
		headers = StringHeaders(raw)
	}
	if snap == nil && len(headers) == 0 {
		return Event{}, false
	}
	return Event{
		Provider: provider,
		Model:    model,
		Snapshot: snap,
		Headers:  headers,
		At:       time.Now().UTC(),
	}, true
}
