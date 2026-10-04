package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
	"lmgateway/internal/store/mem"
)

func TestChatGPTCodexNativeResponses(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.reasoning_summary_text.delta\n")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n")
	}))
	defer upstream.Close()

	js := mem.NewJSON()
	_, err := js.Put(context.Background(), "auth/chatgpt_codex", codexauth.TokenRecord{
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := codexauth.New(js, codexauth.Config{Issuer: upstream.URL})
	provider := NewChatGPTCodex(ChatGPTCodexConfig{Name: "chatgpt", BaseURL: upstream.URL, Auth: auth})
	doc, err := lm.NewRequest("openai_response", map[string]any{
		"model": "gpt-5.6-luna", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		"stream": true, "store": false, "reasoning": map[string]any{"effort": "max", "summary": "concise"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := provider.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if gotBody["model"] != "gpt-5.6-luna" || gotBody["reasoning"].(map[string]any)["summary"] != "concise" {
		t.Fatalf("native responses body was changed: %v", gotBody)
	}
	response, ok := out.Response()
	if !ok {
		t.Fatal("expected response document")
	}
	value, ok := response.Get("event")
	if !ok {
		t.Fatal("expected response event channel")
	}
	events := value.(<-chan lm.LMEvent)
	event, ok := <-events
	if !ok || lm.EventName(event) != "response.reasoning_summary_text.delta" {
		t.Fatalf("unexpected native event: %T %v", event, ok)
	}
}

func TestChatGPTCodexDiscovery(t *testing.T) {
	var gotOriginator string
	var gotQuery string
	var gotAccountID string
	var gotResidency string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginator = r.Header.Get("originator")
		gotQuery = r.URL.RawQuery
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		gotResidency = r.Header.Get("x-openai-internal-codex-residency")
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Errorf("missing OAuth token")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"slug":"gpt-5.6-sol","owned_by":"openai","additional_speed_tiers":["fast"]}]}`)
	}))
	defer upstream.Close()

	js := mem.NewJSON()
	if _, err := js.Put(context.Background(), "auth/chatgpt_codex", codexauth.TokenRecord{
		AccessToken: "access-token", ExpiresAt: time.Now().Add(time.Hour), AccountID: "account-1", Residency: "eu",
	}); err != nil {
		t.Fatal(err)
	}
	auth := codexauth.New(js, codexauth.Config{Issuer: upstream.URL})
	provider := NewChatGPTCodex(ChatGPTCodexConfig{Name: "chatgpt_codex", BaseURL: upstream.URL, Originator: "opencode", Auth: auth})
	models, err := provider.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotOriginator != "opencode" || gotQuery != "client_version=0.0.0" || gotAccountID != "account-1" || gotResidency != "eu" {
		t.Fatalf("unexpected discovery request: originator=%q query=%q account_id=%q residency=%q", gotOriginator, gotQuery, gotAccountID, gotResidency)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" || len(models[0].SpeedTiers) != 1 || models[0].SpeedTiers[0] != "fast" {
		t.Fatalf("unexpected discovered models: %+v", models)
	}
}

func TestChatGPTCodexResponsesNonStreaming(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.output_item.added", "item": map[string]any{"type": "message", "id": "m1", "role": "assistant", "content": []any{}}},
			{"type": "response.output_text.delta", "item_id": "m1", "delta": "OK"},
			{"type": "response.completed", "response": map[string]any{"id": "r1", "model": "codex-model", "status": "completed", "output": []any{map[string]any{"type": "message", "id": "m1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}}}},
		} {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer upstream.Close()
	js := mem.NewJSON()
	_, err := js.Put(context.Background(), "auth/chatgpt_codex", codexauth.TokenRecord{AccessToken: "access-token", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	auth := codexauth.New(js, codexauth.Config{Issuer: upstream.URL})
	provider := NewChatGPTCodex(ChatGPTCodexConfig{Name: "chatgpt", BaseURL: upstream.URL, Auth: auth})
	doc, err := lm.NewRequest("openai_response", map[string]any{"model": "codex-model", "input": "hello", "stream": false})
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReqDocument(doc)
	pkt.Set(packet.KeyCtx, context.Background())
	out := provider.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	response, ok := out.Response()
	if !ok {
		t.Fatal("missing response document")
	}
	if typ, ok := lm.TypeOf(response); !ok || typ != "openai_response" {
		t.Fatalf("unexpected response document type: %q %v", typ, ok)
	}
	raw, _ := lm.Map(response)
	if raw["object"] != "response" || raw["status"] != "completed" {
		t.Fatalf("unexpected Responses response: %v", raw)
	}
	if gotBody["stream"] != true {
		t.Fatalf("Codex upstream must use streaming transport: %v", gotBody)
	}
}

func TestChatGPTCodexForwardsOAuthAndClientMetadata(t *testing.T) {
	var gotHeaders http.Header
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&gotBody)
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "codex-1", "model": "gpt-5.4"}},
			{"type": "response.output_text.delta", "delta": "OK"},
			{"type": "response.completed", "response": map[string]any{
				"id": "codex-1", "model": "gpt-5.4",
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
			}},
		} {
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	js := mem.NewJSON()
	_, err := js.Put(context.Background(), "auth/chatgpt_codex", codexauth.TokenRecord{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    "account-1",
		Residency:    "eu",
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := codexauth.New(js, codexauth.Config{Issuer: upstream.URL})
	provider := NewChatGPTCodex(ChatGPTCodexConfig{
		Name:                  "chatgpt",
		BaseURL:               upstream.URL,
		Auth:                  auth,
		ForwardClientMetadata: true,
	})

	pkt := packet.NewReq(map[string]any{
		"model":            "chatgpt/gpt-5.4-fast",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "max",
		"max_tokens":       1234,
		"stream_options":   map[string]any{"include_usage": true},
	})
	pkt.Set(packet.KeyCtx, context.Background())
	pkt.Set(packet.KeyHTTPMeta, map[string]any{
		"user_agent": "codex-client/test",
		"session_id": "session-1",
		"originator": "client-originator",
	})

	out := provider.Handle(pkt)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if out.Phase() != packet.PhaseResp {
		t.Fatalf("expected resp phase, got %q", out.Phase())
	}
	resp, _ := out.Map("resp")
	if resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "OK" {
		t.Errorf("responses stream was not assembled: %v", resp)
	}
	if gotHeaders.Get("Authorization") != "Bearer access-token" {
		t.Errorf("missing OAuth authorization: %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("ChatGPT-Account-Id") != "account-1" {
		t.Errorf("missing account id: %q", gotHeaders.Get("ChatGPT-Account-Id"))
	}
	if gotHeaders.Get("x-openai-internal-codex-residency") != "eu" {
		t.Errorf("missing residency: %q", gotHeaders.Get("x-openai-internal-codex-residency"))
	}
	if gotHeaders.Get("User-Agent") != "codex-client/test" || gotHeaders.Get("session_id") != "session-1" {
		t.Errorf("client metadata not forwarded: ua=%q session=%q", gotHeaders.Get("User-Agent"), gotHeaders.Get("session_id"))
	}
	if gotHeaders.Get("Cookie") != "" || gotHeaders.Get("X-Api-Key") != "" {
		t.Error("sensitive inbound headers must not be forwarded")
	}
	if gotBody["model"] != "gpt-5.4" {
		t.Errorf("unexpected body: %v", gotBody)
	}
	if gotBody["service_tier"] != "priority" {
		t.Errorf("fast model should use priority service tier: %v", gotBody)
	}
	if gotBody["stream"] != true || gotBody["store"] != false {
		t.Errorf("codex request should be streamed and not stored: %v", gotBody)
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Errorf("Codex request must omit Chat stream_options: %v", gotBody)
	}
	reasoning, _ := gotBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "max" {
		t.Errorf("reasoning_effort was not translated to Responses reasoning.effort: %v", gotBody)
	}
	if _, ok := gotBody["max_output_tokens"]; ok {
		t.Errorf("Codex request must omit unsupported max_output_tokens: %v", gotBody)
	}
	if _, ok := gotBody["input"]; !ok {
		t.Errorf("codex request missing Responses input: %v", gotBody)
	}
}
