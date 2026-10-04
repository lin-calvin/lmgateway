package lmgcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResourceRequestAndAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/config/settings/patch/spend" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing authorization: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("If-Match") != "4" {
			t.Errorf("missing if-match: %q", r.Header.Get("If-Match"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "secret", time.Second)
	data, err := client.Resource(context.Background(), "setting", "patch", "spend", []byte(`{"timezone":"UTC"}`), "4")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("unexpected response: %s", data)
	}
}

func TestReadDocumentYAML(t *testing.T) {
	data, err := ReadDocument("", "model: gpt-4o\nreasoning:\n  effort: high\n", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"model":"gpt-4o","reasoning":{"effort":"high"}}` && string(data) != `{"reasoning":{"effort":"high"},"model":"gpt-4o"}` {
		t.Fatalf("unexpected JSON: %s", data)
	}
}

func TestHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"version conflict"}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "", time.Second).Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "version conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpErr, ok := err.(*HTTPError); !ok || httpErr.Status != http.StatusConflict {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
}
