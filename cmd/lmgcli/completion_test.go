package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"lmgateway/internal/lmgcli"
)

type stubData struct {
	p, m, s, o, r, a []string
}

func (d stubData) providers() []string { return d.p }
func (d stubData) models() []string    { return d.m }
func (d stubData) settings() []string  { return d.s }
func (d stubData) pools() []string     { return d.o }
func (d stubData) rules() []string     { return d.r }
func (d stubData) aliases() []string   { return d.a }

func completeAt(prior []string, current string, data completionData) []string {
	words := append([]string{"lmgcli"}, prior...)
	words = append(words, current)
	return completeWords(len(words)-1, words, data)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestCompleteCommands(t *testing.T) {
	data := stubData{}
	got := completeAt(nil, "", data)
	if !contains(got, "alias") || !contains(got, "provider") {
		t.Fatalf("missing commands: %v", got)
	}
	got = completeAt(nil, "al", data)
	if !reflect.DeepEqual(got, []string{"alias"}) {
		t.Fatalf("expected alias, got %v", got)
	}
}

func TestCompleteResourceNames(t *testing.T) {
	data := stubData{p: []string{"openai", "deepseek"}, m: []string{"gpt-7", "deepseek-v4"}, s: []string{"spend"}, r: []string{"alias-agent", "other"}}
	if got := completeAt([]string{"provider", "get"}, "", data); !contains(got, "deepseek") {
		t.Fatalf("provider names: %v", got)
	}
	if got := completeAt([]string{"model", "get"}, "deep", data); !reflect.DeepEqual(got, []string{"deepseek-v4"}) {
		t.Fatalf("model names filtered: %v", got)
	}
	if got := completeAt([]string{"rule", "get"}, "", data); !contains(got, "alias-agent") {
		t.Fatalf("rule ids: %v", got)
	}
	if got := completeAt([]string{"setting", "list"}, "", data); got != nil {
		t.Fatalf("list takes no name: %v", got)
	}
}

func TestCompleteAlias(t *testing.T) {
	data := stubData{m: []string{"gpt-7", "gpt-8"}, a: []string{"agent", "agent-max"}}

	if got := completeAt([]string{"alias"}, "", data); !contains(got, "unset") {
		t.Fatalf("alias ops: %v", got)
	}
	if got := completeAt([]string{"alias", "get"}, "agent", data); !contains(got, "agent-max") {
		t.Fatalf("alias names: %v", got)
	}
	// KEY position after NAME
	if got := completeAt([]string{"alias", "set", "agent"}, "", data); !contains(got, "model") || !contains(got, "effort") {
		t.Fatalf("alias keys: %v", got)
	}
	// VALUE position for model -> merged model names
	if got := completeAt([]string{"alias", "set", "agent", "model"}, "gpt", data); !reflect.DeepEqual(got, []string{"gpt-7", "gpt-8"}) {
		t.Fatalf("model values: %v", got)
	}
	// VALUE position for effort / summary
	if got := completeAt([]string{"alias", "set", "agent", "effort"}, "m", data); !reflect.DeepEqual(got, []string{"medium", "max"}) {
		t.Fatalf("effort values: %v", got)
	}
	if got := completeAt([]string{"alias", "set", "agent", "summary"}, "", data); !contains(got, "concise") {
		t.Fatalf("summary values: %v", got)
	}
	// free-form value -> no candidates
	if got := completeAt([]string{"alias", "set", "agent", "top_p"}, "", data); got != nil {
		t.Fatalf("top_p should not suggest: %v", got)
	}
	// second KEY after a completed pair
	if got := completeAt([]string{"alias", "set", "agent", "model", "gpt-7"}, "", data); !contains(got, "effort") {
		t.Fatalf("second key: %v", got)
	}
}

func TestRunCompleteAgainstAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/model/list":
			_, _ = w.Write([]byte(`{"items":[{"name":"gpt-7"},{"name":"deepseek-v4"}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chatgpt_codex/gpt-5.6-luna"},{"id":"glm_codingplan/glm-5.3"}]}`))
		case "/api/config/rules/list":
			_, _ = w.Write([]byte(`{"rules":[{"id":"alias-agent"},{"id":"alias-agent-max"},{"id":"other"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := lmgcli.NewClient(server.URL, "", 3*time.Second)
	run := func(cword int, words []string) []string {
		var buf bytes.Buffer
		args := append([]string{strconv.Itoa(cword)}, "lmgcli")
		args = append(args, words...)
		if code := runComplete(context.Background(), client, args, &buf); code != 0 {
			t.Fatalf("runComplete returned %d", code)
		}
		return strings.Fields(buf.String())
	}

	// model names merge configured + discovered models.
	models := run(3, []string{"model", "get", ""})
	if !contains(models, "gpt-7") || !contains(models, "chatgpt_codex/gpt-5.6-luna") || !contains(models, "glm_codingplan/glm-5.3") {
		t.Fatalf("expected configured and discovered models, got %v", models)
	}
	// alias names come from the rules list.
	aliases := run(3, []string{"alias", "get", ""})
	if !reflect.DeepEqual(aliases, []string{"agent", "agent-max"}) {
		t.Fatalf("alias names: %v", aliases)
	}
	// alias model values use the merged model list.
	values := run(5, []string{"alias", "set", "agent", "model", ""})
	if !contains(values, "glm_codingplan/glm-5.3") {
		t.Fatalf("alias model values should include discovered models: %v", values)
	}
}

func TestCompletePool(t *testing.T) {
	data := stubData{o: []string{"deepseek-v4.1", "glm-5.3"}}
	if got := completeAt([]string{"pool"}, "", data); !contains(got, "rotate") || !contains(got, "status") {
		t.Fatalf("pool ops: %v", got)
	}
	if got := completeAt([]string{"pool", "status"}, "deep", data); !reflect.DeepEqual(got, []string{"deepseek-v4.1"}) {
		t.Fatalf("pool status names: %v", got)
	}
	if got := completeAt([]string{"pool", "get"}, "", data); !contains(got, "glm-5.3") {
		t.Fatalf("pool names: %v", got)
	}
}

func TestCompleteFlags(t *testing.T) {
	if got := completeAt(nil, "--o", stubData{}); !contains(got, "--output") {
		t.Fatalf("flags: %v", got)
	}
	if got := completeFlag("--output="); !reflect.DeepEqual(got, []string{"json", "yaml", "raw"}) {
		t.Fatalf("output values: %v", got)
	}
	if got := completeFlag("--output=j"); !reflect.DeepEqual(got, []string{"json"}) {
		t.Fatalf("output value filter: %v", got)
	}
	if got := completeFlag("--auth-header="); !contains(got, "X-API-Key") {
		t.Fatalf("auth header values: %v", got)
	}
}
