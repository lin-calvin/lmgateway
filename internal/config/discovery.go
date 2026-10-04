package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProviderModel 从 provider /v1/models 发现到的模型（未加前缀）
type ProviderModel struct {
	ID         string
	OwnedBy    string
	Created    int64
	SpeedTiers []string
}

// ModelEntry /v1/models 列表项：显式模型无前缀，发现模型带 [provider]/ 前缀
type ModelEntry struct {
	ID         string
	OwnedBy    string
	Created    int64
	SpeedTiers []string
}

// ModelsFetcher 每次实时热取 provider 的 /v1/models，不做缓存
type ModelsFetcher struct {
	client *http.Client
}

func NewModelsFetcher() *ModelsFetcher {
	return &ModelsFetcher{client: &http.Client{Timeout: 10 * time.Second}}
}

// Fetch 拉取单个 provider 的模型目录。
// 仅支持 OpenAI-compatible（chatgpt_codex 等后续由各自适配）。
func (f *ModelsFetcher) Fetch(ctx context.Context, p ProviderCfg) ([]ProviderModel, error) {
	if p.Type != "openai" || p.BaseURL == "" {
		return nil, fmt.Errorf("provider %s: model discovery not supported for type %q", p.Name, p.Type)
	}
	key := p.APIKey
	if key == "" && p.APIKeyEnv != "" {
		key = os.Getenv(p.APIKeyEnv)
	}
	endpoint := strings.TrimSuffix(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider %s: models returned %d", p.Name, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("provider %s: decode models failed: %w", p.Name, err)
	}
	models := make([]ProviderModel, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, ProviderModel{ID: m.ID, OwnedBy: m.OwnedBy, Created: m.Created})
	}
	return models, nil
}
