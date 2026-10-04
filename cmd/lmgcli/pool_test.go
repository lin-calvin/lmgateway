package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lmgateway/internal/lmgcli"
)

func TestExecutePool(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := lmgcli.NewClient(server.URL, "", time.Second)
	ctx := context.Background()

	if _, err := executePool(ctx, client, "status", []string{"pooled"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/pool/status/pooled" {
		t.Fatalf("status -> %s %s", method, path)
	}
	if _, err := executePool(ctx, client, "rotate", []string{"pooled"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/pool/rotate/pooled" {
		t.Fatalf("rotate -> %s %s", method, path)
	}
	if _, err := executePool(ctx, client, "clear", []string{"pooled"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/pool/clear/pooled" {
		t.Fatalf("clear -> %s %s", method, path)
	}
	if _, err := executePool(ctx, client, "list", nil); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/pool/list" {
		t.Fatalf("list -> %s %s", method, path)
	}
	if _, err := executePool(ctx, client, "rotate", nil); err == nil {
		t.Fatal("rotate without a model should error")
	}
}
