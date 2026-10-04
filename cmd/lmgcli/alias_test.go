package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/lmgcli"
)

func TestAliasAssignments(t *testing.T) {
	sets, defaults, err := aliasAssignments([]string{"model", "gpt7", "effort", "max", "reasoning.summary", "concise", "temperature", "0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if sets["model"] != "gpt7" {
		t.Fatalf("model should be a hard set: %v", sets)
	}
	if defaults["reasoning.effort"] != "max" {
		t.Fatalf("effort shorthand: %v", defaults)
	}
	if defaults["reasoning.summary"] != "concise" {
		t.Fatalf("dotted path: %v", defaults)
	}
	if defaults["temperature"] != 0.5 {
		t.Fatalf("temperature should parse as number: %v", defaults)
	}
	if _, ok := sets["reasoning.effort"]; ok {
		t.Fatalf("effort must not be hard: %v", sets)
	}
}

func TestAliasAssignmentsRejectsBareUnknownKey(t *testing.T) {
	if _, _, err := aliasAssignments([]string{"effrot", "max"}); err == nil {
		t.Fatal("expected an error for an unknown bare key")
	}
}

func TestAliasRuleBody(t *testing.T) {
	body := aliasRuleBody("alias-agent", "agent", map[string]any{"model": "gpt7"}, map[string]any{"reasoning.effort": "max"})
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var rule map[string]any
	if err := json.Unmarshal(encoded, &rule); err != nil {
		t.Fatal(err)
	}
	if rule["id"] != "alias-agent" || rule["from"] != "http" || rule["to"] != "http" {
		t.Fatalf("unexpected envelope: %v", rule)
	}
	match, _ := rule["match"].([]any)
	cond, _ := match[0].(map[string]any)
	if cond["field"] != "model" || cond["op"] != "eq" || cond["value"] != "agent" {
		t.Fatalf("unexpected match: %v", cond)
	}
	if _, ok := rule["default"]; !ok {
		t.Fatalf("defaults should be present: %v", rule)
	}
}

func TestParseAliasArgs(t *testing.T) {
	positionals, ifMatch, ruleID, err := parseAliasArgs([]string{"agent", "model", "gpt7", "--if-match", "3", "--rule-id=alias-custom"})
	if err != nil {
		t.Fatal(err)
	}
	if ifMatch != "3" || ruleID != "alias-custom" {
		t.Fatalf("flags not parsed: ifMatch=%q ruleID=%q", ifMatch, ruleID)
	}
	want := []string{"agent", "model", "gpt7"}
	if len(positionals) != len(want) {
		t.Fatalf("positionals: %v", positionals)
	}
	for i := range want {
		if positionals[i] != want[i] {
			t.Fatalf("positionals: %v", positionals)
		}
	}
}

func TestAliasRuleID(t *testing.T) {
	if aliasRuleID("agent") != "alias-agent" {
		t.Fatal("should prefix")
	}
	if aliasRuleID("alias-agent") != "alias-agent" {
		t.Fatal("should pass through")
	}
}

func TestAliasSetMergesExistingRule(t *testing.T) {
	var written map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/config/rules/get/alias-agent":
			_, _ = w.Write([]byte(`{"id":"alias-agent","from":"http","match":[{"field":"model","op":"eq","value":"agent"}],"set":{"model":"old"},"default":{"reasoning.effort":"high"},"to":"http","source":"yaml","version":2}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/config/rules/set/alias-agent":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &written)
			_, _ = w.Write([]byte(`{"ok":true,"id":"alias-agent","version":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := lmgcli.NewClient(server.URL, "", time.Second)
	if _, err := executeAlias(context.Background(), client, "set", []string{"agent", "model", "gpt7", "summary", "concise"}); err != nil {
		t.Fatal(err)
	}
	if written == nil {
		t.Fatal("no rule was written")
	}
	sets, _ := written["set"].(map[string]any)
	if sets["model"] != "gpt7" {
		t.Fatalf("model not retargeted: %v", sets)
	}
	defaults, _ := written["default"].(map[string]any)
	if defaults["reasoning.effort"] != "high" {
		t.Fatalf("existing default should be preserved: %v", defaults)
	}
	if defaults["reasoning.summary"] != "concise" {
		t.Fatalf("new default missing: %v", defaults)
	}
}

func TestAliasSetRequiresModelWhenNew(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := lmgcli.NewClient(server.URL, "", time.Second)
	if _, err := executeAlias(context.Background(), client, "set", []string{"agent", "effort", "max"}); err == nil {
		t.Fatal("expected an error when creating an alias without a model")
	}
}
