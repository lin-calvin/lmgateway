package balance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchJSONPathAndString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth header = %q", got)
		}
		w.Write([]byte(`{"data":{"assets":"42.5"}}`))
	}))
	defer srv.Close()

	funded, value, err := Fetch(context.Background(), Config{
		Type: "custom", Path: srv.URL, JSONPath: "data.assets",
	}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !funded || value != 42.5 {
		t.Fatalf("funded=%v value=%v", funded, value)
	}
}

func TestFetchBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"balance_infos":[{"total_balance":"0.00"}]}`))
	}))
	defer srv.Close()

	funded, value, err := Fetch(context.Background(), Config{
		Type: "custom", Path: srv.URL, JSONPath: "balance_infos.0.total_balance",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if funded || value != 0 {
		t.Fatalf("zero balance should be unfunded: funded=%v value=%v", funded, value)
	}
}

func TestPresetDefaults(t *testing.T) {
	p, ok := presets["deepseek"]
	if !ok || p.jsonPath != "balance_infos.0.total_balance" {
		t.Fatalf("deepseek preset wrong: %+v", p)
	}
	if pr := presets["autodl"]; pr.method != http.MethodPost {
		t.Fatalf("autodl should be POST: %+v", pr)
	}
}

func TestAutodlRawTokenAndPost(t *testing.T) {
	var gotMethod, gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAuth, gotCT = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.Write([]byte(`{"data":{"assets":3}}`))
	}))
	defer srv.Close()

	_, value, err := Fetch(context.Background(), Config{Type: "autodl", Path: srv.URL}, "devtok")
	if err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatalf("value=%v", value)
	}
	if gotMethod != http.MethodPost || gotAuth != "devtok" || gotCT != "application/json" {
		t.Fatalf("autodl request: method=%s auth=%q ct=%q", gotMethod, gotAuth, gotCT)
	}
}

func TestOpenRouterSubtractsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"total_credits":10,"total_usage":12}}`))
	}))
	defer srv.Close()

	funded, value, err := Fetch(context.Background(), Config{Type: "openrouter", Path: srv.URL}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if funded || value != -2 {
		t.Fatalf("expected remaining -2 (unfunded), got funded=%v value=%v", funded, value)
	}
}

func TestFunded(t *testing.T) {
	if !Funded(1, 0) || Funded(0, 0) {
		t.Fatal("below=0 should mean >0")
	}
	if !Funded(5, 5) || Funded(4.9, 5) {
		t.Fatal("below=5 should mean >=5")
	}
}
