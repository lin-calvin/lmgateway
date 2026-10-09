package pool

import "testing"

func TestControllerStickyAndRotate(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b", "c"}, CooldownSec: 60})

	// sticky: repeated lookups keep the first backend
	for i := 0; i < 3; i++ {
		if got, ok := c.Active("m"); !ok || got != "a" {
			t.Fatalf("expected sticky a, got %q ok=%v", got, ok)
		}
	}
	c.Report("m", "a", "rate_limit", 0)
	if got, _ := c.Active("m"); got != "b" {
		t.Fatalf("expected rotation to b, got %q", got)
	}
	// a second report for the already-cooling backend must not move active again
	c.Report("m", "a", "rate_limit", 0)
	if got, _ := c.Active("m"); got != "b" {
		t.Fatalf("active should stay b, got %q", got)
	}
	c.Report("m", "b", "rate_limit", 0)
	if got, _ := c.Active("m"); got != "c" {
		t.Fatalf("expected rotation to c, got %q", got)
	}
	c.Report("m", "c", "rate_limit", 0)
	if _, ok := c.Active("m"); ok {
		t.Fatal("all backends cooling should fail")
	}
	if c.RetryAfter("m") <= 0 {
		t.Fatal("expected a positive retry-after while limited")
	}
}

func TestControllerForceLeastRecent(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60, OnAllLimited: "force-least-recent"})
	c.Report("m", "a", "rate_limit", 0)
	c.Report("m", "b", "rate_limit", 0)
	got, ok := c.Active("m")
	if !ok || (got != "a" && got != "b") {
		t.Fatalf("force-least-recent should return a backend, got %q ok=%v", got, ok)
	}
}

func TestRegisterPreservesRuntimeState(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60})
	c.Report("m", "a", "rate_limit", 0)
	if got, _ := c.Active("m"); got != "b" {
		t.Fatalf("expected b, got %q", got)
	}
	// a hot reload re-registers the same pool; active must not reset
	c.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60})
	if got, _ := c.Active("m"); got != "b" {
		t.Fatalf("re-register must preserve active, got %q", got)
	}
	// removing the active backend promotes a valid one
	c.Register(Config{Model: "m", Backend: []string{"b"}, CooldownSec: 60})
	if got, ok := c.Active("m"); !ok || got != "b" {
		t.Fatalf("expected b after shrinking backends, got %q ok=%v", got, ok)
	}
}

func TestControllerRotateAndClear(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b", "c"}, CooldownSec: 60})

	if active, ok := c.Rotate("m"); !ok || active != "b" {
		t.Fatalf("rotate should advance to b, got %q ok=%v", active, ok)
	}
	if _, ok := c.Rotate("unknown"); ok {
		t.Fatal("rotate of an unknown pool should return ok=false")
	}
	if !c.ClearCooldowns("m") {
		t.Fatal("clear of a known pool should return true")
	}
	if c.ClearCooldowns("unknown") {
		t.Fatal("clear of an unknown pool should return false")
	}
}

func TestPolicySortAndReselect(t *testing.T) {
	// speed_first prefers the faster backend even if pricier.
	speed := NewController()
	speed.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60,
		Policy: PolicySpeedFirst, ReselectTTFTMs: 100, Price: map[string]float64{"a": 5, "b": 1}})
	speed.ObserveStats("m", "a", 1000, 0)
	speed.ObserveStats("m", "b", 8000, 0)
	if got, _ := speed.Active("m"); got != "a" {
		t.Fatalf("speed_first should keep the faster a, got %q", got)
	}

	// price_first prefers the cheaper backend even if slower.
	price := NewController()
	price.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60,
		Policy: PolicyPriceFirst, ReselectTTFTMs: 100, Price: map[string]float64{"a": 5, "b": 1}})
	price.ObserveStats("m", "a", 1000, 0)
	price.ObserveStats("m", "b", 8000, 0)
	if got, _ := price.Active("m"); got != "b" {
		t.Fatalf("price_first should switch to the cheaper b, got %q", got)
	}
}

func TestReselectOnSlowAndUnknown(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60,
		Policy: PolicySpeedFirst, ReselectTTFTMs: 5000})
	// active defaults to a; a becomes slow -> reselect to the faster b.
	c.ObserveStats("m", "a", 6000, 0)
	c.ObserveStats("m", "b", 1200, 0)
	if got, _ := c.Active("m"); got != "b" {
		t.Fatalf("expected reselect to b, got %q", got)
	}
	// unknown ttft sorts last: fresh pool, only b observed -> switches off a
	c2 := NewController()
	c2.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60,
		Policy: PolicySpeedFirst, ReselectTTFTMs: 5000})
	c2.ObserveStats("m", "b", 500, 0)
	// a is unknown (ttft +Inf) and active; it exceeds the cap, so switch to b.
	if got, _ := c2.Active("m"); got != "b" {
		t.Fatalf("unknown-ttft active should reselect to observed b, got %q", got)
	}
}

func TestObserveStatsSkipSemantics(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a"}, CooldownSec: 60})
	c.ObserveStats("m", "a", 1000, 0) // ttft sample
	c.ObserveStats("m", "a", -1, 0.5) // cost-only update, keep ttft
	status, _ := c.Status("m")
	st := status.Stats["a"]
	if st.Samples != 1 || st.TTFTMs != 1000 {
		t.Fatalf("ttft sample should be unchanged: %+v", st)
	}
	if st.Cost != 0.5 {
		t.Fatalf("cost should be updated: %+v", st)
	}
}

func TestStatus(t *testing.T) {
	c := NewController()
	c.Register(Config{Model: "m", Backend: []string{"a", "b"}, CooldownSec: 60})
	c.Report("m", "a", "rate_limit", 0)
	status, ok := c.Status("m")
	if !ok || status.Active != "b" || status.Cooling["a"] <= 0 {
		t.Fatalf("unexpected status: %+v ok=%v", status, ok)
	}
}
