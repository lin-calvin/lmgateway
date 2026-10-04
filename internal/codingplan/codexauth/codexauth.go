// Package codexauth implements the ChatGPT subscription OAuth flow used by
// OpenCode's Codex provider. It deliberately stores tokens behind JSONStore
// and never returns access or refresh tokens from its public status methods.
package codexauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lmgateway/internal/store"
)

const (
	DefaultIssuer = "https://auth.openai.com"
	DefaultClient = "app_EMoamEEZ73f0CkXaXp7hrann"
	authKey       = "auth/chatgpt_codex"
	tokenSkew     = 60 * time.Second
)

var (
	ErrNoAuth      = errors.New("codex auth: not authenticated")
	ErrUnknownFlow = errors.New("codex auth: unknown or expired login flow")
)

// Config controls OAuth endpoints. Only device login is supported.
type Config struct {
	Issuer   string
	ClientID string
}

func (c Config) normalized() Config {
	if c.Issuer == "" {
		c.Issuer = DefaultIssuer
	}
	if c.ClientID == "" {
		c.ClientID = DefaultClient
	}
	return c
}

type TokenRecord struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id,omitempty"`
	Residency    string    `json:"residency,omitempty"`
}

type Status struct {
	Authenticated bool      `json:"authenticated"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	AccountID     string    `json:"account_id,omitempty"`
	Residency     string    `json:"residency,omitempty"`
}

type DeviceChallenge struct {
	RequestID       string        `json:"request_id"`
	VerificationURL string        `json:"verification_url"`
	UserCode        string        `json:"user_code"`
	Interval        time.Duration `json:"-"`
	ExpiresAt       time.Time     `json:"expires_at"`
	deviceAuthID    string
}

type DeviceResult struct {
	Status          string    `json:"status"` // pending | authenticated
	RequestID       string    `json:"request_id"`
	VerificationURL string    `json:"verification_url,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	AccountID       string    `json:"account_id,omitempty"`
}

type Service struct {
	store  store.JSONStore
	cfg    Config
	client *http.Client

	mu      sync.Mutex
	devices map[string]DeviceChallenge
}

func New(s store.JSONStore, cfg Config) *Service {
	return &Service{
		store:   s,
		cfg:     cfg.normalized(),
		client:  &http.Client{Timeout: 30 * time.Second},
		devices: map[string]DeviceChallenge{},
	}
}

// StartDevice begins OpenAI's headless device flow.
func (s *Service) StartDevice(ctx context.Context) (DeviceChallenge, error) {
	var out struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"`
	}
	if err := s.postJSON(ctx, s.cfg.Issuer+"/api/accounts/deviceauth/usercode", map[string]any{
		"client_id": s.cfg.ClientID,
	}, &out); err != nil {
		return DeviceChallenge{}, err
	}
	if out.DeviceAuthID == "" || out.UserCode == "" {
		return DeviceChallenge{}, errors.New("codex auth: device response missing device_auth_id/user_code")
	}
	interval := 5 * time.Second
	if n, err := strconv.Atoi(out.Interval); err == nil && n > 0 {
		interval = time.Duration(n) * time.Second
	}
	requestID, err := randomString(18)
	if err != nil {
		return DeviceChallenge{}, err
	}
	c := DeviceChallenge{
		RequestID:       requestID,
		VerificationURL: s.cfg.Issuer + "/codex/device",
		UserCode:        out.UserCode,
		Interval:        interval,
		ExpiresAt:       time.Now().Add(15 * time.Minute),
		deviceAuthID:    out.DeviceAuthID,
	}
	s.mu.Lock()
	s.devices[requestID] = c
	s.mu.Unlock()
	return c, nil
}

// PollDevice polls once. The caller can poll at the returned interval.
func (s *Service) PollDevice(ctx context.Context, requestID string) (DeviceResult, error) {
	s.mu.Lock()
	c, ok := s.devices[requestID]
	s.mu.Unlock()
	if !ok || time.Now().After(c.ExpiresAt) {
		return DeviceResult{}, ErrUnknownFlow
	}

	resp, status, err := s.postJSONStatus(ctx, s.cfg.Issuer+"/api/accounts/deviceauth/token", map[string]any{
		"device_auth_id": c.deviceAuthID,
		"user_code":      c.UserCode,
	})
	if err != nil {
		if status == http.StatusForbidden || status == http.StatusNotFound {
			return DeviceResult{
				Status:          "pending",
				RequestID:       c.RequestID,
				VerificationURL: c.VerificationURL,
				UserCode:        c.UserCode,
				ExpiresAt:       c.ExpiresAt,
			}, nil
		}
		return DeviceResult{}, err
	}

	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(resp, &code); err != nil {
		return DeviceResult{}, err
	}
	if code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return DeviceResult{}, errors.New("codex auth: device token response missing authorization code")
	}
	tokens, err := s.exchangeCode(ctx, code.AuthorizationCode, code.CodeVerifier, s.cfg.Issuer+"/deviceauth/callback")
	if err != nil {
		return DeviceResult{}, err
	}
	if err := s.save(ctx, tokens); err != nil {
		return DeviceResult{}, err
	}
	s.mu.Lock()
	delete(s.devices, requestID)
	s.mu.Unlock()
	return DeviceResult{Status: "authenticated", RequestID: requestID, AccountID: tokens.AccountID}, nil
}

// GetAccessToken returns a valid access token and refreshes synchronously.
// The mutex prevents concurrent refresh races and refresh-token rotation loss.
func (s *Service) GetAccessToken(ctx context.Context) (TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := TokenRecord{}
	d, err := s.store.Get(ctx, authKey)
	if err == store.ErrNotFound {
		return t, ErrNoAuth
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(d.Data, &t); err != nil {
		return t, err
	}
	if t.AccessToken != "" && time.Now().Before(t.ExpiresAt.Add(-tokenSkew)) {
		return t, nil
	}
	if t.RefreshToken == "" {
		return TokenRecord{}, ErrNoAuth
	}
	refreshed, err := s.refresh(ctx, t.RefreshToken)
	if err != nil {
		return TokenRecord{}, err
	}
	if err := s.saveUnlocked(ctx, refreshed); err != nil {
		return TokenRecord{}, err
	}
	return refreshed, nil
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	d, err := s.store.Get(ctx, authKey)
	if err == store.ErrNotFound {
		return Status{Authenticated: false}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var t TokenRecord
	if err := json.Unmarshal(d.Data, &t); err != nil {
		return Status{}, err
	}
	return Status{Authenticated: t.AccessToken != "" && time.Now().Before(t.ExpiresAt), ExpiresAt: t.ExpiresAt, AccountID: t.AccountID, Residency: t.Residency}, nil
}

func (s *Service) Logout(ctx context.Context) error { return s.store.Delete(ctx, authKey) }

func (s *Service) exchangeCode(ctx context.Context, code, verifier, redirect string) (TokenRecord, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {s.cfg.ClientID},
		"code_verifier": {verifier},
	}
	body, status, err := s.postFormStatus(ctx, s.cfg.Issuer+"/oauth/token", form)
	if err != nil {
		return TokenRecord{}, fmt.Errorf("codex auth: token exchange failed (%d): %w", status, err)
	}
	return parseTokenRecord(body)
}

func (s *Service) refresh(ctx context.Context, refreshToken string) (TokenRecord, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {s.cfg.ClientID},
	}
	body, status, err := s.postFormStatus(ctx, s.cfg.Issuer+"/oauth/token", form)
	if err != nil {
		return TokenRecord{}, fmt.Errorf("codex auth: token refresh failed (%d): %w", status, err)
	}
	t, err := parseTokenRecord(body)
	if err != nil {
		return TokenRecord{}, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func (s *Service) save(ctx context.Context, t TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveUnlocked(ctx, t)
}

func (s *Service) saveUnlocked(ctx context.Context, t TokenRecord) error {
	_, err := s.store.Put(ctx, authKey, t)
	return err
}

func parseTokenRecord(body []byte) (TokenRecord, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return TokenRecord{}, err
	}
	if raw.AccessToken == "" || raw.IDToken == "" {
		return TokenRecord{}, errors.New("codex auth: token response missing access_token/id_token")
	}
	expires := time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	if raw.ExpiresIn <= 0 {
		if exp, ok := jwtNumberClaim(raw.AccessToken, "exp"); ok {
			expires = time.Unix(int64(exp), 0)
		}
	}
	account := jwtStringClaim(raw.IDToken, "https://api.openai.com/auth", "chatgpt_account_id")
	if account == "" {
		account = jwtStringClaim(raw.AccessToken, "https://api.openai.com/auth", "chatgpt_account_id")
	}
	residency := jwtStringClaim(raw.AccessToken, "https://api.openai.com/auth", "chatgpt_compute_residency")
	if residency == "no_constraint" {
		residency = ""
	}
	return TokenRecord{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, IDToken: raw.IDToken, ExpiresAt: expires, AccountID: account, Residency: residency}, nil
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

func jwtStringClaim(token, namespace, key string) string {
	claims := jwtPayload(token)
	if claims == nil {
		return ""
	}
	if nested, ok := claims[namespace].(map[string]any); ok {
		if v, ok := nested[key].(string); ok {
			return v
		}
	}
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

func jwtNumberClaim(token, key string) (float64, bool) {
	claims := jwtPayload(token)
	v, ok := claims[key].(float64)
	return v, ok
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	body, _, err := s.postJSONStatus(ctx, endpoint, payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (s *Service) postJSONStatus(ctx context.Context, endpoint string, payload any) ([]byte, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

func (s *Service) postFormStatus(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}
