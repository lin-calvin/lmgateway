package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"lmgateway/internal/lmgcli"
)

const version = "0.1.0"

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	configPath := argumentValue(args, "--config")
	if configPath == "" {
		configPath = envOr("LMGCLI_CONFIG", lmgcli.DefaultConfigPath())
	}
	fileConfig, err := lmgcli.LoadConfig(lmgcli.ExpandPath(configPath))
	if err != nil {
		return fail(stderr, 2, "load config: "+err.Error())
	}
	resolved, timeout, err := lmgcli.ResolveConfig(fileConfig, os.Getenv)
	if err != nil {
		return fail(stderr, 2, err.Error())
	}
	apiKey, err := lmgcli.ReadAPIKey(resolved, os.Getenv)
	if err != nil {
		return fail(stderr, 2, "read API key: "+err.Error())
	}
	fs := flag.NewFlagSet("lmgcli", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", resolved.Server, "lmgateway management API URL")
	directAPIKey := fs.String("api-key", "", "API key")
	apiKeyFile := fs.String("api-key-file", "", "read API key from file")
	authHeader := fs.String("auth-header", resolved.AuthHeader, "authentication header: Authorization or X-API-Key")
	output := fs.String("output", resolved.Output, "output format: json, yaml, raw")
	configFile := fs.String("config", configPath, "lmgcli TOML config path")
	timeoutFlag := fs.Duration("timeout", timeout, "HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 || fs.Arg(0) == "help" {
		printUsage(stdout)
		return 0
	}
	if fs.Arg(0) == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if fs.Arg(0) == "config" && fs.NArg() >= 2 && fs.Arg(1) == "init" {
		path := argumentValue(fs.Args()[2:], "--path")
		if path == "" {
			path = *configFile
		}
		path = lmgcli.ExpandPath(path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fail(stderr, 2, "create config directory: "+err.Error())
		}
		if err := os.WriteFile(path, lmgcli.Template(), 0o600); err != nil {
			return fail(stderr, 2, "write config: "+err.Error())
		}
		fmt.Fprintln(stdout, path)
		return 0
	}
	if *directAPIKey != "" {
		apiKey = *directAPIKey
	}
	if *apiKeyFile != "" {
		data, err := os.ReadFile(lmgcli.ExpandPath(*apiKeyFile))
		if err != nil {
			return fail(stderr, 2, "read API key file: "+err.Error())
		}
		apiKey = strings.TrimSpace(string(data))
	}
	client := lmgcli.NewClient(*server, apiKey, *timeoutFlag)
	client.AuthHead = *authHeader
	ctx := context.Background()
	commandArgs := fs.Args()
	data, err := execute(ctx, client, commandArgs)
	if err != nil {
		return fail(stderr, exitCode(err), err.Error())
	}
	formatted, err := lmgcli.Format(data, *output)
	if err != nil {
		return fail(stderr, 2, err.Error())
	}
	_, _ = stdout.Write(append(formatted, '\n'))
	return 0
}

func execute(ctx context.Context, client *lmgcli.Client, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing command")
	}
	if args[0] == "health" {
		return client.Health(ctx)
	}
	if args[0] == "action" {
		if len(args) != 2 {
			return nil, fmt.Errorf("usage: lmgcli action NAME")
		}
		return client.Action(ctx, args[1], nil)
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("missing operation for %s", args[0])
	}
	kind, operation := args[0], args[1]
	if kind == "config" && operation == "reload" {
		return client.Action(ctx, "config/reload", nil)
	}
	if kind == "rule" {
		return executeRule(ctx, client, operation, args[2:])
	}
	if kind != "provider" && kind != "model" && kind != "setting" {
		return nil, fmt.Errorf("unknown command %q", kind)
	}
	name, body, ifMatch, err := parseResourceArgs(args[2:], operation)
	if err != nil {
		return nil, err
	}
	if kind == "setting" && operation == "delete" {
		return nil, fmt.Errorf("settings do not support delete")
	}
	return client.Resource(ctx, kind, operation, name, body, ifMatch)
}

func executeRule(ctx context.Context, client *lmgcli.Client, operation string, args []string) ([]byte, error) {
	if operation == "meta" || operation == "list" {
		return client.Rule(ctx, operation, "", nil, "")
	}
	if operation == "set" || operation == "patch" {
		if hasDirectRuleArgs(args) {
			return executeDirectRule(ctx, client, operation, args)
		}
	}
	id, body, ifMatch, err := parseResourceArgs(args, operation)
	if err != nil {
		return nil, err
	}
	return client.Rule(ctx, operation, id, body, ifMatch)
}

func hasDirectRuleArgs(args []string) bool {
	for _, arg := range args {
		for _, name := range []string{"--from", "--match", "--set", "--action", "--to"} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func executeDirectRule(ctx context.Context, client *lmgcli.Client, operation string, args []string) ([]byte, error) {
	fs := flag.NewFlagSet("rule", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "rule source")
	action := fs.String("action", "", "rule action")
	to := fs.String("to", "", "next rule source")
	ifMatch := fs.String("if-match", "", "expected resource version")
	var matches stringFlags
	var sets stringFlags
	fs.Var(&matches, "match", "rule matcher, field=value or field:op=value")
	fs.Var(&sets, "set", "rule assignment, path=value")
	args = normalizeResourceArgs(args)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		return nil, fmt.Errorf("rule %s requires exactly one rule id", operation)
	}
	id := fs.Arg(0)
	if operation == "patch" {
		return patchRuleDirect(ctx, client, id, *from, *action, *to, matches, sets, *ifMatch)
	}
	if *from == "" {
		return nil, fmt.Errorf("rule set requires --from")
	}
	body, err := directRuleBody(id, *from, *action, *to, matches, sets)
	if err != nil {
		return nil, err
	}
	return client.Rule(ctx, "set", id, body, *ifMatch)
}

func patchRuleDirect(ctx context.Context, client *lmgcli.Client, id, from, action, to string, matches, sets stringFlags, ifMatch string) ([]byte, error) {
	current, err := client.Rule(ctx, "get", id, nil, "")
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(current, &body); err != nil {
		return nil, err
	}
	delete(body, "source")
	delete(body, "version")
	if from != "" {
		body["from"] = from
	}
	if action != "" {
		body["action"] = action
	}
	if to != "" {
		body["to"] = to
	}
	if len(matches) > 0 {
		compiled, err := parseRuleMatches(matches)
		if err != nil {
			return nil, err
		}
		body["match"] = compiled
	}
	if len(sets) > 0 {
		assignments, err := parseRuleSets(sets)
		if err != nil {
			return nil, err
		}
		currentSet, _ := body["set"].(map[string]any)
		if currentSet == nil {
			currentSet = map[string]any{}
		}
		for key, value := range assignments {
			currentSet[key] = value
		}
		body["set"] = currentSet
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return client.Rule(ctx, "set", id, encoded, ifMatch)
}

func directRuleBody(id, from, action, to string, matches, sets stringFlags) ([]byte, error) {
	compiled, err := parseRuleMatches(matches)
	if err != nil {
		return nil, err
	}
	assignments, err := parseRuleSets(sets)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"id":    id,
		"from":  from,
		"match": compiled,
		"set":   assignments,
	}
	if action != "" {
		body["action"] = action
	}
	if to != "" {
		body["to"] = to
	}
	return json.Marshal(body)
}

func parseRuleMatches(values stringFlags) ([]map[string]any, error) {
	matches := make([]map[string]any, 0, len(values))
	for _, value := range values {
		left, right, ok := strings.Cut(value, "=")
		if !ok || left == "" {
			return nil, fmt.Errorf("invalid --match %q, expected field=value or field:op=value", value)
		}
		field, op := left, "eq"
		if name, value, ok := strings.Cut(left, ":"); ok {
			field, op = name, value
		}
		if field == "" || op == "" {
			return nil, fmt.Errorf("invalid --match %q", value)
		}
		parsed := parseCLIValue(right)
		item := map[string]any{"field": field, "op": op, "value": parsed}
		if op == "in" {
			item["values"] = parsed
			delete(item, "value")
		}
		matches = append(matches, item)
	}
	return matches, nil
}

func parseRuleSets(values stringFlags) (map[string]any, error) {
	sets := map[string]any{}
	for _, value := range values {
		key, raw, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --set %q, expected path=value", value)
		}
		sets[key] = parseCLIValue(raw)
	}
	return sets, nil
}

func parseCLIValue(value string) any {
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) == nil {
		return parsed
	}
	return value
}

type stringFlags []string

func (f *stringFlags) String() string { return strings.Join(*f, ",") }
func (f *stringFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseResourceArgs(args []string, operation string) (string, []byte, string, error) {
	fs := flag.NewFlagSet("resource", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "input JSON or YAML file")
	value := fs.String("json", "", "input JSON value")
	stdin := fs.Bool("stdin", false, "read input from stdin")
	ifMatch := fs.String("if-match", "", "expected resource version")
	args = normalizeResourceArgs(args)
	if err := fs.Parse(args); err != nil {
		return "", nil, "", err
	}
	name := ""
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	needsName := operation != "list"
	if needsName && name == "" {
		return "", nil, "", fmt.Errorf("missing resource name")
	}
	if !needsName && name != "" {
		return "", nil, "", fmt.Errorf("unexpected resource name %q", name)
	}
	if operation != "set" && operation != "patch" && (*file != "" || *value != "" || *stdin) {
		return "", nil, "", fmt.Errorf("input is only valid for set or patch")
	}
	if operation != "set" && operation != "patch" {
		return name, nil, *ifMatch, nil
	}
	if (*file != "" && *value != "") || (*file != "" && *stdin) || (*value != "" && *stdin) {
		return "", nil, "", fmt.Errorf("use only one of --file, --json, or --stdin")
	}
	if *file == "" && *value == "" && !*stdin {
		return "", nil, "", fmt.Errorf("set or patch requires --file, --json, or --stdin")
	}
	body, err := lmgcli.ReadDocument(*file, *value, os.Stdin)
	return name, body, *ifMatch, err
}

func normalizeResourceArgs(args []string) []string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return args
	}
	return append(append([]string(nil), args[1:]...), args[0])
}

func argumentValue(args []string, name string) string {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func exitCode(err error) int {
	if httpErr, ok := err.(*lmgcli.HTTPError); ok {
		switch {
		case httpErr.Status == 401 || httpErr.Status == 403:
			return 3
		case httpErr.Status == 404:
			return 4
		case httpErr.Status == 409:
			return 5
		case httpErr.Status == 400 || httpErr.Status == 422:
			return 6
		case httpErr.Status == 502:
			return 7
		case httpErr.Status >= 500:
			return 9
		}
	}
	return 8
}

func fail(stderr io.Writer, code int, message string) int {
	fmt.Fprintln(stderr, message)
	return code
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "lmgcli - lmgateway management API client")
	fmt.Fprintln(w, "Usage: lmgcli [global options] COMMAND OPERATION [NAME] [options]")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  health")
	fmt.Fprintln(w, "  provider|model|setting list|get|set|patch|delete|reset NAME")
	fmt.Fprintln(w, "  rule meta|list|get|set|patch|delete|reset ID")
	fmt.Fprintln(w, "  action NAME")
	fmt.Fprintln(w, "  config reload")
	fmt.Fprintln(w, "Write options: --file FILE | --json JSON | --stdin, --if-match VERSION")
	fmt.Fprintln(w, "Rule options: --from SOURCE, --match field[ :op]=value, --set path=value, --action NAME, --to SOURCE")
	fmt.Fprintln(w, "Global options: --server URL, --api-key KEY, --api-key-file FILE, --output json|yaml|raw")
}
