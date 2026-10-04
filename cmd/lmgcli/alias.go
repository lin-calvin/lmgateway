package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"lmgateway/internal/lmgcli"
)

// An alias is a rule with the convention id "alias-<name>":
//
//	from: http
//	match: [{ field: model, op: eq, value: <name> }]
//	set:     { model: <target> }          # hard: routes the alias
//	default: { reasoning.effort: max, … } # soft: caller can override
//	to: http
const aliasRulePrefix = "alias-"

// aliasKeyPaths maps the natural-language keys of the positional grammar to
// request paths written as soft defaults.
var aliasKeyPaths = map[string]string{
	"effort":      "reasoning.effort",
	"summary":     "reasoning.summary",
	"temperature": "temperature",
	"top_p":       "top_p",
	"max_tokens":  "max_tokens",
}

// aliasKeys is the completion / usage vocabulary (model is the hard target).
var aliasKeys = []string{"model", "effort", "summary", "temperature", "top_p", "max_tokens"}

func aliasRuleID(name string) string {
	if strings.HasPrefix(name, aliasRulePrefix) {
		return name
	}
	return aliasRulePrefix + name
}

func isNotFound(err error) bool {
	var httpErr *lmgcli.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == 404
}

func executeAlias(ctx context.Context, client *lmgcli.Client, operation string, args []string) ([]byte, error) {
	switch operation {
	case "list":
		return aliasList(ctx, client)
	case "get":
		return aliasGet(ctx, client, args)
	case "set":
		return aliasSet(ctx, client, args)
	case "unset":
		return aliasUnset(ctx, client, args)
	case "reset", "delete":
		return aliasSimple(ctx, client, operation, args)
	default:
		return nil, fmt.Errorf("unknown alias operation %q (want list|get|set|unset|reset|delete)", operation)
	}
}

func aliasList(ctx context.Context, client *lmgcli.Client) ([]byte, error) {
	raw, err := client.Rule(ctx, "list", "", nil, "")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(payload.Rules))
	for _, rule := range payload.Rules {
		id, _ := rule["id"].(string)
		if !strings.HasPrefix(id, aliasRulePrefix) {
			continue
		}
		item := map[string]any{"name": strings.TrimPrefix(id, aliasRulePrefix)}
		if sets, ok := rule["set"].(map[string]any); ok {
			if model, ok := sets["model"]; ok {
				item["model"] = model
			}
		}
		if defaults, ok := rule["default"].(map[string]any); ok && len(defaults) > 0 {
			item["default"] = defaults
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return json.Marshal(map[string]any{"aliases": out})
}

func aliasGet(ctx context.Context, client *lmgcli.Client, args []string) ([]byte, error) {
	positionals, _, overrideID, err := parseAliasArgs(args)
	if err != nil {
		return nil, err
	}
	if len(positionals) != 1 {
		return nil, errors.New("usage: lmgcli alias get NAME")
	}
	return client.Rule(ctx, "get", aliasTargetID(positionals[0], overrideID), nil, "")
}

func aliasSet(ctx context.Context, client *lmgcli.Client, args []string) ([]byte, error) {
	positionals, ifMatch, overrideID, err := parseAliasArgs(args)
	if err != nil {
		return nil, err
	}
	if len(positionals) < 1 {
		return nil, errors.New("usage: lmgcli alias set NAME [KEY VALUE]...")
	}
	name := positionals[0]
	pairs := positionals[1:]
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("expected KEY VALUE pairs after %q, got an odd number of tokens", name)
	}
	ruleID := aliasTargetID(name, overrideID)
	aliasName := strings.TrimPrefix(ruleID, aliasRulePrefix)

	sets, defaults, err := aliasAssignments(pairs)
	if err != nil {
		return nil, err
	}

	// Merge over the existing rule so updating one default keeps the others.
	existingSet, existingDefault, err := aliasCurrentValues(ctx, client, ruleID)
	if err != nil {
		return nil, err
	}
	for key, value := range existingSet {
		if _, ok := sets[key]; !ok {
			sets[key] = value
		}
	}
	for key, value := range existingDefault {
		if _, ok := defaults[key]; !ok {
			defaults[key] = value
		}
	}
	if _, ok := sets["model"]; !ok {
		return nil, fmt.Errorf("alias %q has no target model; add `model <name>`", aliasName)
	}

	body := aliasRuleBody(ruleID, aliasName, sets, defaults)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return client.Rule(ctx, "set", ruleID, encoded, ifMatch)
}

func aliasUnset(ctx context.Context, client *lmgcli.Client, args []string) ([]byte, error) {
	positionals, ifMatch, overrideID, err := parseAliasArgs(args)
	if err != nil {
		return nil, err
	}
	if len(positionals) < 2 {
		return nil, errors.New("usage: lmgcli alias unset NAME KEY [KEY]...")
	}
	ruleID := aliasTargetID(positionals[0], overrideID)
	raw, err := client.Rule(ctx, "get", ruleID, nil, "")
	if err != nil {
		return nil, err
	}
	var rule map[string]any
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, err
	}
	defaults, _ := rule["default"].(map[string]any)
	if defaults == nil {
		defaults = map[string]any{}
	}
	for _, key := range positionals[1:] {
		if key == "model" {
			return nil, errors.New("cannot unset alias model; use `alias set NAME model <name>` to retarget")
		}
		path, ok := aliasKeyPaths[key]
		if !ok {
			if strings.Contains(key, ".") {
				path = key
			} else {
				return nil, fmt.Errorf("unknown alias key %q", key)
			}
		}
		delete(defaults, path)
	}
	delete(rule, "source")
	delete(rule, "version")
	delete(rule, "name")
	if len(defaults) > 0 {
		rule["default"] = defaults
	} else {
		delete(rule, "default")
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	return client.Rule(ctx, "set", ruleID, encoded, ifMatch)
}

func aliasSimple(ctx context.Context, client *lmgcli.Client, operation string, args []string) ([]byte, error) {
	positionals, ifMatch, overrideID, err := parseAliasArgs(args)
	if err != nil {
		return nil, err
	}
	if len(positionals) != 1 {
		return nil, fmt.Errorf("usage: lmgcli alias %s NAME", operation)
	}
	return client.Rule(ctx, operation, aliasTargetID(positionals[0], overrideID), nil, ifMatch)
}

func aliasTargetID(name, overrideID string) string {
	if overrideID != "" {
		return overrideID
	}
	return aliasRuleID(name)
}

// aliasAssignments turns KEY VALUE... pairs into hard `set` and soft `default`
// maps. `model` is the only hard key; a bare unknown key is rejected, while a
// dotted key falls through as an explicit request path.
func aliasAssignments(pairs []string) (map[string]any, map[string]any, error) {
	sets := map[string]any{}
	defaults := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, value := pairs[i], pairs[i+1]
		if key == "model" {
			sets["model"] = parseCLIValue(value)
			continue
		}
		path, ok := aliasKeyPaths[key]
		if !ok {
			if !strings.Contains(key, ".") {
				return nil, nil, fmt.Errorf("unknown alias key %q (known: %s, or a dotted path)", key, strings.Join(aliasKeys, ", "))
			}
			path = key
		}
		defaults[path] = parseCLIValue(value)
	}
	return sets, defaults, nil
}

func aliasRuleBody(ruleID, aliasName string, sets, defaults map[string]any) map[string]any {
	body := map[string]any{
		"id":    ruleID,
		"from":  "http",
		"match": []any{map[string]any{"field": "model", "op": "eq", "value": aliasName}},
		"set":   sets,
		"to":    "http",
	}
	if len(defaults) > 0 {
		body["default"] = defaults
	}
	return body
}

func aliasCurrentValues(ctx context.Context, client *lmgcli.Client, ruleID string) (map[string]any, map[string]any, error) {
	raw, err := client.Rule(ctx, "get", ruleID, nil, "")
	if isNotFound(err) {
		return map[string]any{}, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var rule map[string]any
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, nil, err
	}
	sets, _ := rule["set"].(map[string]any)
	defaults, _ := rule["default"].(map[string]any)
	if sets == nil {
		sets = map[string]any{}
	}
	if defaults == nil {
		defaults = map[string]any{}
	}
	return sets, defaults, nil
}

// parseAliasArgs separates positional tokens from the optional flags, which may
// appear anywhere (the global parser stops at the first positional).
func parseAliasArgs(args []string) (positionals []string, ifMatch, ruleID string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--if-match":
			if i+1 >= len(args) {
				return nil, "", "", errors.New("--if-match requires a value")
			}
			i++
			ifMatch = args[i]
		case strings.HasPrefix(arg, "--if-match="):
			ifMatch = strings.TrimPrefix(arg, "--if-match=")
		case arg == "--rule-id":
			if i+1 >= len(args) {
				return nil, "", "", errors.New("--rule-id requires a value")
			}
			i++
			ruleID = args[i]
		case strings.HasPrefix(arg, "--rule-id="):
			ruleID = strings.TrimPrefix(arg, "--rule-id=")
		case strings.HasPrefix(arg, "-"):
			return nil, "", "", fmt.Errorf("unknown flag %q", arg)
		default:
			positionals = append(positionals, arg)
		}
	}
	return positionals, ifMatch, ruleID, nil
}
