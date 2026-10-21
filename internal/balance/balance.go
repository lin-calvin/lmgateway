// Package balance reads an account's remaining balance from a provider's
// balance API, so the pool can recover a backend that was marked dead by a
// quota/balance error once the account is funded again. It is not a proactive
// gate — callers only use it to check recovery.
package balance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config selects the endpoint and how to read a number out of the response.
type Config struct {
	Type         string  // preset name ("deepseek", "autodl", …) or "custom"
	Path         string  // override endpoint URL/path
	JSONPath     string  // override JSON path to the value
	SubtractJSON string  // optional: funded value = pluck(JSONPath) - pluck(SubtractJSON)
	Below        float64 // see Funded
}

// preset is a built-in reader: endpoint, method, value path, and auth style.
type preset struct {
	method       string
	path         string
	jsonPath     string
	subtractJSON string
	rawToken     bool // send the key as-is in Authorization (no "Bearer " prefix)
}

var presets = map[string]preset{
	// GET https://api.deepseek.com/user/balance → balance_infos[0].total_balance
	"deepseek": {method: http.MethodGet, path: "https://api.deepseek.com/user/balance", jsonPath: "balance_infos.0.total_balance"},
	// POST https://api.autodl.com/api/v1/dev/wallet/balance → data.assets.
	// AutoDL wants the raw developer token in Authorization (no Bearer) + a JSON body.
	"autodl": {method: http.MethodPost, path: "https://api.autodl.com/api/v1/dev/wallet/balance", jsonPath: "data.assets", rawToken: true},
	// GET https://openrouter.ai/api/v1/credits → total_credits - total_usage.
	"openrouter": {method: http.MethodGet, path: "https://openrouter.ai/api/v1/credits", jsonPath: "data.total_credits", subtractJSON: "data.total_usage"},
	// GET https://api.siliconflow.cn/v1/user/info → data.totalBalance
	"siliconflow": {method: http.MethodGet, path: "https://api.siliconflow.cn/v1/user/info", jsonPath: "data.totalBalance"},
}

// Fetch calls the configured balance endpoint and returns whether the account is
// funded plus the parsed value.
func Fetch(ctx context.Context, cfg Config, apiKey string) (funded bool, value float64, err error) {
	typ := strings.ToLower(strings.TrimSpace(cfg.Type))
	p, ok := presets[typ]
	if !ok {
		if typ != "custom" && typ != "" {
			return false, 0, fmt.Errorf("unknown balance_type %q", cfg.Type)
		}
		p = preset{method: http.MethodGet}
	}
	method := p.method
	if method == "" {
		method = http.MethodGet
	}
	url := cfg.Path
	if url == "" {
		url = p.path
	}
	jsonPath := cfg.JSONPath
	if jsonPath == "" {
		jsonPath = p.jsonPath
	}
	subtract := cfg.SubtractJSON
	if subtract == "" {
		subtract = p.subtractJSON
	}
	if url == "" || jsonPath == "" {
		return false, 0, fmt.Errorf("balance: path and jsonpath are required")
	}

	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		if p.rawToken {
			req.Header.Set("Authorization", apiKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, 0, fmt.Errorf("balance endpoint returned %d", resp.StatusCode)
	}
	var doc any
	if err := json.Unmarshal(respBody, &doc); err != nil {
		return false, 0, fmt.Errorf("decode balance response: %w", err)
	}
	value, ok = number(pluck(doc, jsonPath))
	if !ok {
		return false, 0, fmt.Errorf("balance jsonpath %q not found", jsonPath)
	}
	if subtract != "" {
		if sub, ok := number(pluck(doc, subtract)); ok {
			value -= sub
		}
	}
	return Funded(value, cfg.Below), value, nil
}

// Funded reports whether a value meets the threshold. below<=0 means ">0 is funded".
func Funded(value, below float64) bool {
	if below <= 0 {
		return value > 0
	}
	return value >= below
}

// pluck walks a dotted path ("a.b.0.c") over maps and array indices.
func pluck(doc any, path string) any {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
	}
	return cur
}

// number coerces JSON numbers and numeric strings to float64.
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}
