// Package compat applies a small, declarative request transform table for
// provider API quirks. It is intentionally not a general scripting language.
package compat

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed transform.json
var transformJSON []byte

type Scope struct {
	Provider string
	Model    string
	Dialect  string
}

type rule struct {
	Name  string `json:"name"`
	Match match  `json:"match"`
	Ops   []op   `json:"ops"`
}

type match struct {
	Provider []string `json:"provider,omitempty"`
	Model    []string `json:"model,omitempty"`
	Dialect  []string `json:"dialect,omitempty"`
}

type op struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	From  string `json:"from,omitempty"`
	Value *value `json:"value,omitempty"`
}

type value struct {
	From        string         `json:"from,omitempty"`
	Literal     any            `json:"literal,omitempty"`
	Map         map[string]any `json:"map,omitempty"`
	Default     any            `json:"default,omitempty"`
	Passthrough bool           `json:"passthrough,omitempty"`
}

type transformFile struct {
	Version int    `json:"version"`
	Rules   []rule `json:"rules"`
}

var rules []rule

func init() {
	var file transformFile
	if err := json.Unmarshal(transformJSON, &file); err != nil {
		panic("compat: invalid transform.json: " + err.Error())
	}
	rules = file.Rules
}

// Apply mutates raw according to the rules matching scope. Unknown ops are
// ignored by validation at init time; malformed paths simply do nothing.
func Apply(raw map[string]any, scope Scope) error {
	if raw == nil || scope.Dialect == "" {
		return nil
	}
	canonical := canonicalValues(raw)
	for _, r := range rules {
		if !r.Match.matches(scope) {
			continue
		}
		for _, o := range r.Ops {
			if err := o.apply(raw, canonical); err != nil {
				return fmt.Errorf("compat rule %s: %w", r.Name, err)
			}
		}
	}
	return nil
}

func (m match) matches(scope Scope) bool {
	return matchAny(m.Provider, scope.Provider) &&
		matchAny(m.Model, scope.Model) &&
		matchAny(m.Dialect, scope.Dialect)
}

func matchAny(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if glob(pattern, value) {
			return true
		}
	}
	return false
}

func glob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(value, parts[i])
		if idx < 0 {
			return false
		}
		value = value[idx+len(parts[i]):]
	}
	return strings.HasSuffix(value, parts[len(parts)-1])
}

func canonicalValues(raw map[string]any) map[string]any {
	out := map[string]any{}
	reasoning := map[string]any{}
	if v, ok := raw["reasoning_effort"]; ok {
		reasoning["effort"] = v
	}
	if v, ok := raw["reasoning_summary"]; ok {
		reasoning["summary"] = v
	}
	if nested, ok := raw["reasoning"].(map[string]any); ok {
		for _, key := range []string{"effort", "summary", "budget_tokens"} {
			if v, ok := nested[key]; ok {
				reasoning[key] = v
			}
		}
	}
	if len(reasoning) > 0 {
		out["reasoning"] = reasoning
	}
	if v, ok := raw["max_output_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := raw["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	return out
}

func (o op) apply(raw, canonical map[string]any) error {
	switch o.Op {
	case "set", "set_if":
		value, ok := o.resolve(canonical)
		if !ok {
			return nil
		}
		setPath(raw, o.Path, value)
	case "set_default":
		if _, ok := getPath(raw, o.Path); ok {
			return nil
		}
		if value, ok := o.resolve(canonical); ok {
			setPath(raw, o.Path, value)
		}
	case "delete":
		deletePath(raw, o.Path)
	case "rename":
		value, ok := getPath(raw, o.From)
		if !ok {
			return nil
		}
		setPath(raw, o.Path, value)
		deletePath(raw, o.From)
	default:
		return fmt.Errorf("unknown op %q", o.Op)
	}
	return nil
}

func (o op) resolve(canonical map[string]any) (any, bool) {
	if o.Value == nil {
		if o.From == "" {
			return nil, false
		}
		return getPath(canonical, o.From)
	}
	v := o.Value
	if v.From != "" {
		value, ok := getPath(canonical, v.From)
		if !ok {
			return nil, false
		}
		key := fmt.Sprint(value)
		if mapped, ok := v.Map[key]; ok {
			return mapped, true
		}
		if v.Passthrough || (len(v.Map) == 0 && v.Default == nil) {
			return value, true
		}
		if v.Default != nil {
			return v.Default, true
		}
		return nil, false
	}
	if v.Literal != nil {
		return v.Literal, true
	}
	return nil, false
}

func getPath(m map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	var current any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func setPath(m map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := m
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	parents := []map[string]any{m}
	current := m
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return
		}
		current = next
		parents = append(parents, current)
	}
	delete(current, parts[len(parts)-1])
	// Remove empty parent objects created solely by this transform.
	for i := len(parents) - 1; i > 0; i-- {
		if len(parents[i]) == 0 {
			delete(parents[i-1], parts[i-1])
		}
	}
}
