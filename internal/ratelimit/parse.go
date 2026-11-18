package ratelimit

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseGeneric parses standard ratelimit-* / x-ratelimit-* headers. Suffixed
// families such as ratelimit-limit-requests are kept as separate windows.
func ParseGeneric(headers map[string]string) *Snapshot {
	h := lower(headers)
	snap := &Snapshot{
		LimitID:    "default",
		Raw:        filterRateHeaders(h),
		ObservedAt: time.Now().UTC(),
	}
	type bucket struct{ limit, remaining, reset, resetAfter string }
	buckets := map[string]*bucket{}
	get := func(suffix string) *bucket {
		b := buckets[suffix]
		if b == nil {
			b = &bucket{}
			buckets[suffix] = b
		}
		return b
	}
	for name, value := range h {
		lower := strings.ToLower(name)
		var rest string
		switch {
		case strings.HasPrefix(lower, "x-ratelimit-"):
			rest = strings.TrimPrefix(lower, "x-ratelimit-")
		case strings.HasPrefix(lower, "ratelimit-"):
			rest = strings.TrimPrefix(lower, "ratelimit-")
		default:
			continue
		}
		kind, suffix := splitRateKind(rest)
		if kind == "" {
			continue
		}
		b := get(suffix)
		switch kind {
		case "limit":
			b.limit = value
		case "remaining":
			b.remaining = value
		case "reset":
			b.reset = value
		case "reset-after":
			b.resetAfter = value
		}
	}

	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := buckets[name]
		limit, okLimit := parseFloat(b.limit)
		remaining, okRemaining := parseFloat(b.remaining)
		if !okLimit || limit <= 0 || !okRemaining {
			continue
		}
		window := Window{
			Name:          windowName(name),
			Limit:         limit,
			Remaining:     remaining,
			ResetAt:       parseReset(b.reset),
			ResetAfterSec: parseSeconds(b.resetAfter),
		}
		window.UsedPercent = clampPercent((limit - remaining) / limit * 100)
		window.RemainingPercent = clampPercent(remaining / limit * 100)
		snap.Windows = append(snap.Windows, window)
	}
	if len(snap.Windows) == 0 {
		return nil
	}
	return snap
}

func splitRateKind(rest string) (kind, suffix string) {
	for _, k := range []string{"reset-after", "reset", "remaining", "limit"} {
		if rest == k {
			return k, ""
		}
		if strings.HasPrefix(rest, k+"-") {
			return k, strings.TrimPrefix(rest, k+"-")
		}
	}
	return "", ""
}

func windowName(suffix string) string {
	if suffix == "" {
		return "default"
	}
	return suffix
}

// ParseCodexHeaders parses the Codex x-codex-* header families. It returns one
// snapshot per limit id (the default "codex" plus any additional families).
func ParseCodexHeaders(headers map[string]string) []Snapshot {
	h := lower(headers)
	raw := filterRateHeaders(h)
	now := time.Now().UTC()

	defaultSnap := &Snapshot{
		LimitID:     "codex",
		PlanType:    strings.TrimSpace(h["x-codex-plan-type"]),
		ActiveLimit: strings.TrimSpace(h["x-codex-active-limit"]),
		LimitName:   strings.TrimSpace(h["x-codex-limit-name"]),
		Credits:     parseCodexCredits(h),
		Raw:         raw,
		ObservedAt:  now,
	}
	defaultSnap.Windows = codexWindows(h, "x-codex")
	out := []Snapshot{}
	if hasSnapshotData(defaultSnap) {
		out = append(out, *defaultSnap)
	}

	// Additional families: x-codex-<limit>-primary-used-percent.
	families := map[string]bool{}
	for name := range h {
		const suffix = "-primary-used-percent"
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(name, suffix)
		if prefix == "x-codex" || !strings.HasPrefix(prefix, "x-codex-") {
			continue
		}
		families[strings.TrimPrefix(prefix, "x-codex-")] = true
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, family := range names {
		prefix := "x-codex-" + family
		snap := &Snapshot{
			LimitID:     "codex_" + strings.ReplaceAll(family, "-", "_"),
			PlanType:    defaultSnap.PlanType,
			ActiveLimit: defaultSnap.ActiveLimit,
			LimitName:   strings.TrimSpace(h[prefix+"-limit-name"]),
			Credits:     defaultSnap.Credits,
			Raw:         raw,
			ObservedAt:  now,
		}
		snap.Windows = codexWindows(h, prefix)
		if hasSnapshotData(snap) {
			out = append(out, *snap)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func codexWindows(h map[string]string, prefix string) []Window {
	out := []Window{}
	for _, which := range []string{"primary", "secondary"} {
		used, ok := parseFloat(h[prefix+"-"+which+"-used-percent"])
		if !ok {
			continue
		}
		window := Window{
			Name:             which,
			UsedPercent:      clampPercent(used),
			RemainingPercent: clampPercent(100 - used),
			WindowMinutes:    parseInt(h[prefix+"-"+which+"-window-minutes"]),
			ResetAt:          parseReset(h[prefix+"-"+which+"-reset-at"]),
			ResetAfterSec:    parseSeconds(h[prefix+"-"+which+"-reset-after-seconds"]),
		}
		out = append(out, window)
	}
	return out
}

func parseCodexCredits(h map[string]string) *Credits {
	has, ok := parseBool(h["x-codex-credits-has-credits"])
	if !ok {
		return nil
	}
	unlimited, _ := parseBool(h["x-codex-credits-unlimited"])
	return &Credits{
		HasCredits: has,
		Unlimited:  unlimited,
		Balance:    strings.TrimSpace(h["x-codex-credits-balance"]),
	}
}

func hasSnapshotData(s *Snapshot) bool {
	return len(s.Windows) > 0 || s.Credits != nil || s.PlanType != "" || s.LimitName != ""
}

// ParseCodexEvent parses a `codex.rate_limits` SSE event payload.
func ParseCodexEvent(payload []byte) (*Snapshot, bool) {
	var event struct {
		Type        string `json:"type"`
		PlanType    string `json:"plan_type"`
		MeteredName string `json:"metered_limit_name"`
		LimitName   string `json:"limit_name"`
		RateLimits  *struct {
			Primary   *codexEventWindow `json:"primary"`
			Secondary *codexEventWindow `json:"secondary"`
		} `json:"rate_limits"`
		Credits *struct {
			HasCredits bool   `json:"has_credits"`
			Unlimited  bool   `json:"unlimited"`
			Balance    string `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Type != "codex.rate_limits" {
		return nil, false
	}
	snap := &Snapshot{
		LimitID:    "codex",
		PlanType:   event.PlanType,
		LimitName:  firstNonEmpty(event.MeteredName, event.LimitName),
		ObservedAt: time.Now().UTC(),
	}
	if event.RateLimits != nil {
		for name, w := range map[string]*codexEventWindow{"primary": event.RateLimits.Primary, "secondary": event.RateLimits.Secondary} {
			if w == nil {
				continue
			}
			window := Window{
				Name:             name,
				UsedPercent:      clampPercent(w.UsedPercent),
				RemainingPercent: clampPercent(100 - w.UsedPercent),
				WindowMinutes:    w.WindowMinutes,
				ResetAt:          unixTime(w.ResetAt),
			}
			snap.Windows = append(snap.Windows, window)
		}
	}
	if event.Credits != nil {
		snap.Credits = &Credits{HasCredits: event.Credits.HasCredits, Unlimited: event.Credits.Unlimited, Balance: event.Credits.Balance}
	}
	if !hasSnapshotData(snap) {
		return nil, false
	}
	return snap, true
}

type codexEventWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetAt       int64   `json:"reset_at"`
}

func lower(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

func filterRateHeaders(h map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range h {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "ratelimit") || strings.HasPrefix(lower, "x-codex") || strings.HasPrefix(lower, "anthropic-ratelimit") {
			out[lower] = value
		}
	}
	return out
}

func parseFloat(s string) (float64, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

func parseInt(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func parseSeconds(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func parseReset(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return unixTime(n)
	}
	for _, layout := range []string{httpTimeFormat, time.RFC3339, time.RFC1123} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

const httpTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

func unixTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
