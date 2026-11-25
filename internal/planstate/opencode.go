package planstate

import (
	"encoding/json"
	"time"
)

// OpenCode Go exposes its allowance at GET /zen/go/v1/usage. The response has
// three rolling windows; percent is already 0..100 and status is "ok" or
// "rate-limited".
type openCodeUsage struct {
	Usage struct {
		Rolling openCodeMeter `json:"rolling"`
		Weekly  openCodeMeter `json:"weekly"`
		Monthly openCodeMeter `json:"monthly"`
	} `json:"usage"`
}

type openCodeMeter struct {
	Status   string    `json:"status"`
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resetsAt"`
}

// ParseOpenCodeUsage converts the usage endpoint body into a provider-neutral
// snapshot with rolling (5h), weekly and monthly windows.
func ParseOpenCodeUsage(data []byte) (*Snapshot, bool) {
	var body openCodeUsage
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, false
	}
	windows := []Window{}
	for _, item := range []struct {
		name    string
		minutes int
		meter   openCodeMeter
	}{
		{"rolling", 5 * 60, body.Usage.Rolling},
		{"weekly", 7 * 24 * 60, body.Usage.Weekly},
		{"monthly", 30 * 24 * 60, body.Usage.Monthly},
	} {
		if item.meter.Status == "" && item.meter.Percent == 0 && item.meter.ResetsAt.IsZero() {
			continue
		}
		windows = append(windows, Window{
			Name:             item.name,
			Status:           item.meter.Status,
			WindowMinutes:    item.minutes,
			UsedPercent:      clampPercent(item.meter.Percent),
			RemainingPercent: clampPercent(100 - item.meter.Percent),
			ResetAt:          item.meter.ResetsAt.UTC(),
		})
	}
	if len(windows) == 0 {
		return nil, false
	}
	return &Snapshot{
		LimitID:    "opencode",
		LimitName:  "OpenCode Go",
		PlanType:   "go",
		Windows:    windows,
		ObservedAt: time.Now().UTC(),
	}, true
}
