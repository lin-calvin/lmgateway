package compat

import "testing"

func TestDeepSeekDialect(t *testing.T) {
	raw := map[string]any{"reasoning_effort": "medium"}
	if err := Apply(raw, Scope{Dialect: "deepseek"}); err != nil {
		t.Fatal(err)
	}
	thinking, _ := raw["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || raw["reasoning_effort"] != "high" {
		t.Fatalf("raw = %v", raw)
	}

	raw = map[string]any{"reasoning_effort": "none"}
	if err := Apply(raw, Scope{Dialect: "deepseek"}); err != nil {
		t.Fatal(err)
	}
	thinking, _ = raw["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("raw = %v", raw)
	}
}

func TestOpenRouterDialect(t *testing.T) {
	raw := map[string]any{"reasoning_effort": "high", "thinking": map[string]any{"type": "enabled"}}
	if err := Apply(raw, Scope{Dialect: "openrouter"}); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := raw["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("raw = %v", raw)
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Fatalf("top-level reasoning_effort should be removed: %v", raw)
	}
}

func TestQwenDialect(t *testing.T) {
	raw := map[string]any{
		"reasoning_effort": "high",
		"reasoning":        map[string]any{"budget_tokens": 4096},
	}
	if err := Apply(raw, Scope{Dialect: "qwen"}); err != nil {
		t.Fatal(err)
	}
	if raw["enable_thinking"] != true || raw["thinking_budget"] != 4096 {
		t.Fatalf("raw = %v", raw)
	}
}

func TestNoneDialect(t *testing.T) {
	raw := map[string]any{
		"reasoning_effort": "high",
		"reasoning":        map[string]any{"effort": "high"},
		"thinking":         map[string]any{"type": "enabled"},
		"enable_thinking":  true,
	}
	if err := Apply(raw, Scope{Dialect: "none"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "enable_thinking"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("%s should be removed: %v", key, raw)
		}
	}
}

func TestNoDialectIsNoop(t *testing.T) {
	raw := map[string]any{"reasoning_effort": "high", "thinking": map[string]any{"type": "enabled"}}
	before := len(raw)
	if err := Apply(raw, Scope{}); err != nil {
		t.Fatal(err)
	}
	if len(raw) != before {
		t.Fatalf("raw changed: %v", raw)
	}
}
