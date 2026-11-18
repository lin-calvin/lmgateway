package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUsageTimer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{
			"rolling":{"status":"ok","percent":1,"resetsAt":"2026-11-18T19:40:12Z"},
			"weekly":{"status":"ok","percent":2,"resetsAt":"2026-11-23T00:00:00Z"},
			"monthly":{"status":"ok","percent":3,"resetsAt":"2026-12-04T13:58:55Z"}
		}}`))
	}))
	defer upstream.Close()

	usage := &Usage{BaseURL: upstream.URL, APIKey: "test-key"}
	snap, err := usage.Timer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || len(snap.Windows) != 3 || snap.Windows[1].UsedPercent != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
}
