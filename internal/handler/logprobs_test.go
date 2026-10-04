package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
)

func TestLogprobsExporterWebhook(t *testing.T) {
	received := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	exporter := NewLogprobsExporter(LogprobsConfig{WebhookURL: srv.URL})
	pkt := packet.NewReq(map[string]any{"model": "deepseek-v4-flash", "stream": true})
	pkt.Set(packet.KeyProvider, "deepseek")
	pkt.Set(packet.KeyHTTPMeta, map[string]any{"request_id": "req-1", "session_id": "sess-1"})
	pkt.Set(packet.KeyResp, map[string]any{
		"id": "c1", "model": "deepseek-v4-flash",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "hi", "reasoning_content": "think"},
			"logprobs": map[string]any{
				"content": []any{map[string]any{"token": "hi", "logprob": -0.1, "top_logprobs": []any{map[string]any{"token": "hi", "logprob": -0.1}}}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 1.0, "completion_tokens": 1.0},
	})
	exporter.Handle(pkt, dispatch.Serve(func(out packet.Packet, _ ...string) packet.Packet { return out }))

	select {
	case payload := <-received:
		if payload["provider"] != "deepseek" || payload["model"] != "deepseek-v4-flash" {
			t.Fatalf("payload metadata wrong: %v", payload)
		}
		response, _ := payload["response"].(map[string]any)
		if response["content"] != "hi" || response["reasoning"] != "think" {
			t.Fatalf("response content missing: %v", payload)
		}
		logprobs, _ := payload["logprobs"].(map[string]any)
		choices, _ := logprobs["choices"].([]any)
		entries, _ := choices[0].(map[string]any)["content"].([]any)
		if logprobs["format"] != "chat" || len(entries) != 1 {
			t.Fatalf("logprobs missing: %v", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook not called")
	}
}

func TestLogprobsExporterNoReturnedLogprobs(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	exporter := NewLogprobsExporter(LogprobsConfig{WebhookURL: srv.URL})
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyResp, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}}},
	})
	exporter.Handle(pkt, dispatch.Serve(func(out packet.Packet, _ ...string) packet.Packet { return out }))

	select {
	case <-called:
		t.Fatal("webhook should not receive a response without logprobs")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestLogprobsExporterNoWebhook(t *testing.T) {
	exporter := NewLogprobsExporter(LogprobsConfig{})
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyResp, map[string]any{
		"choices": []any{map[string]any{"logprobs": map[string]any{"content": []any{map[string]any{"token": "x"}}}}},
	})
	out := exporter.Handle(pkt, dispatch.Serve(func(out packet.Packet, _ ...string) packet.Packet { return out }))
	if out == nil {
		t.Fatal("exporter must continue without a webhook")
	}
}

func TestLogprobsExporterDropsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	exporter := NewLogprobsExporter(LogprobsConfig{WebhookURL: srv.URL, TimeoutSec: 1})
	pkt := packet.NewReq(map[string]any{"model": "m"})
	pkt.Set(packet.KeyResp, map[string]any{
		"choices": []any{map[string]any{"logprobs": map[string]any{"content": []any{map[string]any{"token": "x"}}}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		exporter.Handle(pkt, dispatch.Serve(func(out packet.Packet, _ ...string) packet.Packet { return out }))
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("exporter blocked request flow")
	}
}
