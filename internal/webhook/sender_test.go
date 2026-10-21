package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSenderEmit(t *testing.T) {
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sender := New(func() Config { return Config{URL: srv.URL, TimeoutSec: 5} })
	if !sender.Enabled() {
		t.Fatal("sender should be enabled")
	}
	sender.Emit(map[string]any{"event": "pool.downgrade", "pool": "p"})

	select {
	case body := <-bodies:
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got["event"] != "pool.downgrade" || got["pool"] != "p" {
			t.Fatalf("unexpected payload: %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was not called")
	}
}

func TestSenderDisabled(t *testing.T) {
	sender := New(func() Config { return Config{} })
	if sender.Enabled() {
		t.Fatal("sender should be disabled")
	}
	sender.Emit(map[string]any{"event": "x"}) // must not panic or block
}
