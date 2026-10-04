package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"lmgateway/internal/lmgcli"
)

const bashCompletionScript = `# bash completion for lmgcli
_lmgcli_completion() {
    local cur out
    cur="${COMP_WORDS[COMP_CWORD]}"
    out="$(lmgcli __complete "${COMP_CWORD}" "${COMP_WORDS[@]}" 2>/dev/null)"
    if [[ -n "${out}" ]]; then
        local IFS=$'\n'
        COMPREPLY=( $(compgen -W "${out}" -- "${cur}") )
    else
        COMPREPLY=()
    fi
}
complete -o default -F _lmgcli_completion lmgcli
`

const zshCompletionScript = `#compdef lmgcli
_lmgcli() {
    local -a completions
    completions=("${(@f)$(lmgcli __complete "$((CURRENT-1))" "${words[@]}" 2>/dev/null)}")
    if (( ${#completions} )); then
        compadd -a completions
    else
        _default
    fi
}
compdef _lmgcli lmgcli
`

var (
	completionCommands = []string{"health", "version", "help", "completion", "provider", "model", "setting", "rule", "alias", "action", "config"}
	resourceOperations = []string{"list", "get", "set", "patch", "delete", "reset"}
	ruleOperations     = []string{"meta", "list", "get", "set", "patch", "delete", "reset"}
	aliasOperations    = []string{"list", "get", "set", "unset", "reset", "delete"}
	completionShells   = []string{"bash", "zsh"}
	globalFlags        = []string{"--server", "--api-key", "--api-key-file", "--auth-header", "--output", "--config", "--timeout", "--help", "-h"}
	effortLevels       = []string{"none", "low", "medium", "high", "max"}
	summaryValues      = []string{"auto", "concise", "detailed"}
)

// completionData supplies the live names used for context-aware completion.
type completionData interface {
	providers() []string
	models() []string
	settings() []string
	rules() []string
	aliases() []string
}

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: lmgcli completion bash|zsh")
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, bashCompletionScript)
	case "zsh":
		fmt.Fprint(stdout, zshCompletionScript)
	default:
		fmt.Fprintf(stderr, "unsupported shell %q (want bash or zsh)\n", args[0])
		return 2
	}
	return 0
}

// runComplete implements the hidden __complete protocol. args are the current
// word index and every word on the command line (including the program name).
// It always returns 0 so a failed lookup never breaks the shell.
func runComplete(ctx context.Context, client *lmgcli.Client, args []string, stdout io.Writer) int {
	if len(args) < 1 {
		return 0
	}
	cword, err := strconv.Atoi(args[0])
	if err != nil || cword < 0 {
		return 0
	}
	words := append([]string(nil), args[1:]...)
	for len(words) <= cword {
		words = append(words, "")
	}
	for _, candidate := range completeWords(cword, words, newAPIData(ctx, client)) {
		fmt.Fprintln(stdout, candidate)
	}
	return 0
}

func completeWords(cword int, words []string, data completionData) []string {
	if cword < 1 || cword >= len(words) {
		return nil
	}
	current := words[cword]
	prior := words[1:cword]

	if strings.HasPrefix(current, "-") {
		return completeFlag(current)
	}

	var candidates []string
	switch {
	case len(prior) == 0:
		candidates = completionCommands
	case prior[0] == "completion":
		if len(prior) == 1 {
			candidates = completionShells
		}
	case prior[0] == "config":
		if len(prior) == 1 {
			candidates = []string{"reload", "init"}
		}
	case prior[0] == "provider" || prior[0] == "model" || prior[0] == "setting":
		candidates = completeResource(prior, data)
	case prior[0] == "rule":
		candidates = completeRule(prior, data)
	case prior[0] == "alias":
		candidates = completeAlias(prior, data)
	}
	return filterPrefix(candidates, current)
}

func completeResource(prior []string, data completionData) []string {
	if len(prior) == 1 {
		if prior[0] == "setting" {
			return []string{"list", "get", "set", "patch", "reset"}
		}
		return resourceOperations
	}
	if len(prior) != 2 || !isNameOperation(prior[1]) {
		return nil
	}
	switch prior[0] {
	case "provider":
		return data.providers()
	case "model":
		return data.models()
	case "setting":
		return data.settings()
	}
	return nil
}

func completeRule(prior []string, data completionData) []string {
	if len(prior) == 1 {
		return ruleOperations
	}
	if len(prior) == 2 && isNameOperation(prior[1]) {
		return data.rules()
	}
	return nil
}

func completeAlias(prior []string, data completionData) []string {
	if len(prior) == 1 {
		return aliasOperations
	}
	operation := prior[1]
	if len(prior) == 2 {
		if isNameOperation(operation) {
			return data.aliases()
		}
		return nil
	}
	if operation != "set" && operation != "unset" {
		return nil
	}
	tail := prior[3:] // tokens after NAME
	if len(tail)%2 == 0 {
		return aliasKeys // completing a KEY
	}
	// completing a VALUE: the previous token is the key
	switch tail[len(tail)-1] {
	case "model":
		return data.models()
	case "effort":
		return effortLevels
	case "summary":
		return summaryValues
	}
	return nil
}

func completeFlag(current string) []string {
	if idx := strings.Index(current, "="); idx >= 0 {
		prefix := current[idx+1:]
		switch current[:idx+1] {
		case "--output=":
			return filterPrefix([]string{"json", "yaml", "raw"}, prefix)
		case "--auth-header=":
			return filterPrefix([]string{"Authorization", "X-API-Key"}, prefix)
		}
		return nil
	}
	return filterPrefix(globalFlags, current)
}

func isNameOperation(operation string) bool {
	switch operation {
	case "get", "set", "patch", "delete", "reset":
		return true
	}
	return false
}

func filterPrefix(candidates []string, prefix string) []string {
	if prefix == "" {
		return candidates
	}
	var out []string
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			out = append(out, candidate)
		}
	}
	return out
}

// ---- live data ----

type apiCompletionData struct {
	ctx    context.Context
	client *lmgcli.Client
	cache  map[string][]string
}

func newAPIData(ctx context.Context, client *lmgcli.Client) *apiCompletionData {
	return &apiCompletionData{ctx: ctx, client: client, cache: map[string][]string{}}
}

func (d *apiCompletionData) providers() []string {
	return d.names("provider", "api/provider/list", "items", "name")
}

func (d *apiCompletionData) settings() []string {
	return d.names("setting", "api/config/settings/list", "items", "name")
}

func (d *apiCompletionData) rules() []string {
	return d.names("rule", "api/config/rules/list", "rules", "id")
}

func (d *apiCompletionData) aliases() []string {
	if cached, ok := d.cache["alias"]; ok {
		return cached
	}
	var out []string
	for _, id := range d.rules() {
		if strings.HasPrefix(id, aliasRulePrefix) {
			out = append(out, strings.TrimPrefix(id, aliasRulePrefix))
		}
	}
	sort.Strings(out)
	d.cache["alias"] = out
	return out
}

// models merges explicitly configured models with live-discovered ones so an
// alias can target a discovered `[provider]/model` id too.
func (d *apiCompletionData) models() []string {
	if cached, ok := d.cache["models"]; ok {
		return cached
	}
	seen := map[string]bool{}
	for _, name := range d.names("model", "api/model/list", "items", "name") {
		seen[name] = true
	}
	if raw, err := d.client.Do(d.ctx, "GET", "v1/models", nil, ""); err == nil {
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			for _, model := range payload.Data {
				if model.ID != "" {
					seen[model.ID] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	d.cache["models"] = out
	return out
}

func (d *apiCompletionData) names(cacheKey, path, arrayField, nameField string) []string {
	if cached, ok := d.cache[cacheKey]; ok {
		return cached
	}
	var out []string
	if raw, err := d.client.Do(d.ctx, "GET", path, nil, ""); err == nil {
		var payload map[string]any
		if json.Unmarshal(raw, &payload) == nil {
			if items, ok := payload[arrayField].([]any); ok {
				for _, item := range items {
					object, _ := item.(map[string]any)
					if name, ok := object[nameField].(string); ok && name != "" {
						out = append(out, name)
					}
				}
			}
		}
	}
	sort.Strings(out)
	d.cache[cacheKey] = out
	return out
}
