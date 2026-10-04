package main

import (
	"encoding/json"
	"testing"
)

func TestDirectRuleBody(t *testing.T) {
	body, err := directRuleBody("alias-agent", "http", "", "http", stringFlags{"model=agent"}, stringFlags{"model=gpt-5.6-luna-fast", "reasoning.effort=high"})
	if err != nil {
		t.Fatal(err)
	}
	var rule map[string]any
	if err := json.Unmarshal(body, &rule); err != nil {
		t.Fatal(err)
	}
	if rule["from"] != "http" || rule["to"] != "http" {
		t.Fatalf("unexpected rule: %v", rule)
	}
	sets, _ := rule["set"].(map[string]any)
	if sets["model"] != "gpt-5.6-luna-fast" || sets["reasoning.effort"] != "high" {
		t.Fatalf("unexpected assignments: %v", sets)
	}
}

func TestParseCLIValue(t *testing.T) {
	if parseCLIValue("true") != true {
		t.Fatal("boolean value was not parsed")
	}
	if parseCLIValue("42") != float64(42) {
		t.Fatal("number value was not parsed")
	}
	if parseCLIValue("high") != "high" {
		t.Fatal("string value was not preserved")
	}
}
