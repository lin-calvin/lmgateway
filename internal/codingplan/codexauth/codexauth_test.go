package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lmgateway/internal/store/mem"
)

func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e." + base64.RawURLEncoding.EncodeToString(b) + ".s"
}

func TestDeviceFlowAndRefresh(t *testing.T) {
	ctx := context.Background()
	access := fakeJWT(map[string]any{
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":        "acct-device",
			"chatgpt_compute_residency": "eu",
		},
	})
	refreshCalls := 0
	devicePolls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-1", "user_code": "ABCD-EFGH", "interval": "1",
			})
		case "/api/accounts/deviceauth/token":
			devicePolls++
			if devicePolls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"authorization_code": "auth-code", "code_verifier": "verifier"})
		case "/oauth/token":
			refreshCalls++
			if r.FormValue("grant_type") == "authorization_code" {
				json.NewEncoder(w).Encode(map[string]any{
					"access_token": access, "refresh_token": "refresh-1", "id_token": fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-device"}}), "expires_in": 3600,
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": access, "refresh_token": "refresh-2", "id_token": fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-refresh"}}), "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := New(mem.NewJSON(), Config{Issuer: server.URL, ClientID: "client-test"})
	challenge, err := service.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "ABCD-EFGH" || challenge.RequestID == "" {
		t.Fatalf("bad challenge: %+v", challenge)
	}
	pending, err := service.PollDevice(ctx, challenge.RequestID)
	if err != nil || pending.Status != "pending" {
		t.Fatalf("expected pending device result: %+v %v", pending, err)
	}
	authenticated, err := service.PollDevice(ctx, challenge.RequestID)
	if err != nil || authenticated.Status != "authenticated" {
		t.Fatalf("expected authenticated device result: %+v %v", authenticated, err)
	}
	status, err := service.Status(ctx)
	if err != nil || !status.Authenticated || status.AccountID != "acct-device" {
		t.Fatalf("bad auth status: %+v %v", status, err)
	}

	// Force refresh through an expired stored record.
	_, err = service.store.Put(ctx, authKey, TokenRecord{AccessToken: "expired", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.GetAccessToken(ctx)
	if err != nil || refreshed.AccessToken != access || refreshed.RefreshToken != "refresh-2" {
		t.Fatalf("refresh failed: %+v %v", refreshed, err)
	}
	if refreshCalls < 2 {
		t.Errorf("expected authorization exchange plus refresh, got %d token calls", refreshCalls)
	}
}

func TestTokenParsing(t *testing.T) {
	token := fakeJWT(map[string]any{
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":        "acct",
			"chatgpt_compute_residency": "no_constraint",
		},
	})
	r, err := parseTokenRecord([]byte(`{"access_token":"` + token + `","refresh_token":"r","id_token":"` + token + `","expires_in":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.AccountID != "acct" || r.Residency != "" || !strings.HasPrefix(r.AccessToken, "e.") {
		t.Fatalf("bad parsed token: %+v", r)
	}
}
