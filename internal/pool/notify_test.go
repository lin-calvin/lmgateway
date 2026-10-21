package pool

import "testing"

func TestNotifierEmitsActiveChange(t *testing.T) {
	c := NewController()
	var got []Event
	c.SetNotifier(func(ev Event) { got = append(got, ev) })
	c.Register(Config{Model: "p", Backend: []string{"a", "b"}, Fallback: "fb", CooldownSec: 60})

	if active, _ := c.Active("p"); active != "a" {
		t.Fatalf("initial active = %q, want a", active)
	}

	// manual rotate a -> b
	if _, ok := c.Rotate("p"); !ok {
		t.Fatal("rotate failed")
	}
	// cool both backends -> downgrade to the fallback
	c.Report("p", "a", "rate_limit", 60)
	c.Report("p", "b", "rate_limit", 60)

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(got), got)
	}
	rotate := got[0]
	if rotate.From != "a" || rotate.To != "b" || rotate.Reason != ReasonRotate || rotate.Downgraded {
		t.Fatalf("unexpected rotate event: %+v", rotate)
	}
	last := got[len(got)-1]
	if last.To != "fb" || !last.Downgraded || last.Model != "p" || last.Reason != ReasonFault || last.Class != "rate_limit" {
		t.Fatalf("unexpected downgrade event: %+v", last)
	}
	if last.Name() != "pool.downgrade" || last.Payload()["reason"] != "fault:rate_limit" {
		t.Fatalf("unexpected downgrade payload: %+v", last.Payload())
	}
	if last.Health["a"] != string(HealthRateLimit) || last.Health["b"] != string(HealthRateLimit) {
		t.Fatalf("health snapshot missing cooldowns: %+v", last.Health)
	}
}

func TestNotifierSkipsFaultOnlyChange(t *testing.T) {
	c := NewController()
	var got []Event
	c.SetNotifier(func(ev Event) { got = append(got, ev) })
	c.Register(Config{Model: "p", Backend: []string{"a", "b"}, CooldownSec: 60})
	c.Active("p") // active = a

	// cooling the non-active backend changes no active target → no event.
	c.Report("p", "b", "rate_limit", 60)
	if len(got) != 0 {
		t.Fatalf("expected no events, got %+v", got)
	}
}
