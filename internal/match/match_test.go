package match

import (
	"testing"

	"lmgateway/internal/packet"
)

func pkt() packet.Packet {
	return packet.Packet{
		"source": "http",
		"req":    map[string]any{"model": "gpt-4o-mini", "stream": false},
		"n":      float64(42),
	}
}

func TestEq(t *testing.T) {
	m := All(Condition{Field: "source", Op: OpEq, Value: "http"})
	if !m.Match(pkt()) {
		t.Error("source=http should match")
	}
	m = All(Condition{Field: "source", Op: OpEq, Value: "openai"})
	if m.Match(pkt()) {
		t.Error("source!=openai should not match")
	}
}

func TestDotPath(t *testing.T) {
	m := All(Condition{Field: "req.model", Op: OpPrefix, Value: "gpt-"})
	if !m.Match(pkt()) {
		t.Error("req.model prefix gpt- should match")
	}
	m = All(Condition{Field: "req.model", Op: OpEq, Value: "gpt-4o-mini"})
	if !m.Match(pkt()) {
		t.Error("req.model exact match should match")
	}
}

func TestIn(t *testing.T) {
	m := All(Condition{Field: "source", Op: OpIn, Value: []any{"openai", "azure", "http"}})
	if !m.Match(pkt()) {
		t.Error("source in set should match")
	}
	m = All(Condition{Field: "source", Op: OpIn, Value: []any{"openai"}})
	if m.Match(pkt()) {
		t.Error("source not in set should not match")
	}
}

func TestExists(t *testing.T) {
	m := All(Condition{Field: "error", Op: OpExists})
	if m.Match(pkt()) {
		t.Error("no error field should not match")
	}
	p := pkt()
	p["error"] = "boom"
	if !m.Match(p) {
		t.Error("error field present should match")
	}
}

func TestRegex(t *testing.T) {
	c, err := Compile("req.model", OpRegex, "^gpt-4")
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	m := All(c)
	if !m.Match(pkt()) {
		t.Error("regex ^gpt-4 should match gpt-4o-mini")
	}
}

func TestMultipleConds(t *testing.T) {
	m := All(
		Condition{Field: "source", Op: OpEq, Value: "http"},
		Condition{Field: "req.stream", Op: OpEq, Value: false},
	)
	if !m.Match(pkt()) {
		t.Error("all conditions satisfied should match")
	}
	m = All(
		Condition{Field: "source", Op: OpEq, Value: "http"},
		Condition{Field: "req.model", Op: OpEq, Value: "claude"},
	)
	if m.Match(pkt()) {
		t.Error("any condition unsatisfied should not match")
	}
}

func TestCompileErrors(t *testing.T) {
	if _, err := Compile("a", Op("bogus"), "x"); err == nil {
		t.Error("unknown op should error")
	}
	if _, err := Compile("a", OpRegex, "("); err == nil {
		t.Error("invalid regex should error")
	}
}
