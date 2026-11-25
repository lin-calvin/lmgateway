package opencode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"lmgateway/internal/planstate"
)

// Usage fetches the OpenCode Go allowance windows. It is a control-plane
// source: OpenCode does not return rate-limit headers on model requests.
type Usage struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func (u *Usage) Timer(ctx context.Context) (*planstate.Snapshot, error) {
	if u == nil || u.APIKey == "" {
		return nil, fmt.Errorf("opencode usage: missing api key")
	}
	base := u.BaseURL
	if base == "" {
		base = "https://opencode.ai"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("opencode usage: invalid base url %q", base)
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	usageURL := scheme + "://" + parsed.Host + "/zen/go/v1/usage"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.APIKey)
	req.Header.Set("Accept", "application/json")
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opencode usage: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	snap, ok := planstate.ParseOpenCodeUsage(body)
	if !ok {
		return nil, fmt.Errorf("opencode usage: empty or invalid response")
	}
	return snap, nil
}
