package lmgcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Client struct {
	BaseURL  string
	APIKey   string
	HTTP     *http.Client
	AuthHead string
}

type HTTPError struct {
	Status int
	Body   json.RawMessage
}

func (e *HTTPError) Error() string {
	if len(e.Body) > 0 {
		var value any
		if json.Unmarshal(e.Body, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				if message, ok := object["error"].(string); ok {
					return fmt.Sprintf("server returned %d: %s", e.Status, message)
				}
				if message, ok := object["message"].(string); ok {
					return fmt.Sprintf("server returned %d: %s", e.Status, message)
				}
			}
		}
		return fmt.Sprintf("server returned %d: %s", e.Status, strings.TrimSpace(string(e.Body)))
	}
	return fmt.Sprintf("server returned %d", e.Status)
}

func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		APIKey:   apiKey,
		HTTP:     &http.Client{Timeout: timeout},
		AuthHead: "Authorization",
	}
}

func (c *Client) Do(ctx context.Context, method, path string, body []byte, ifMatch string) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+strings.TrimLeft(path, "/"), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		if c.AuthHead == "X-API-Key" {
			req.Header.Set("X-API-Key", c.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: append(json.RawMessage(nil), data...)}
	}
	if len(data) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("server returned invalid JSON")
	}
	return json.RawMessage(data), nil
}

func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "healthz", nil, "")
}

func (c *Client) Resource(ctx context.Context, kind, operation, name string, body []byte, ifMatch string) (json.RawMessage, error) {
	var method string
	var path string
	prefix := "api/" + kind
	if kind == "setting" {
		prefix = "api/config/settings"
	}
	switch operation {
	case "list":
		method = http.MethodGet
		path = prefix + "/list"
	case "get":
		method = http.MethodGet
		path = prefix + "/get/" + url.PathEscape(name)
	case "set":
		method = http.MethodPut
		path = prefix + "/set/" + url.PathEscape(name)
	case "patch":
		method = http.MethodPatch
		path = prefix + "/patch/" + url.PathEscape(name)
	case "delete":
		method = http.MethodDelete
		path = prefix + "/delete/" + url.PathEscape(name)
	case "reset":
		method = http.MethodPost
		path = prefix + "/reset/" + url.PathEscape(name)
	default:
		return nil, fmt.Errorf("unknown resource operation %q", operation)
	}
	return c.Do(ctx, method, path, body, ifMatch)
}

func (c *Client) Rule(ctx context.Context, operation, id string, body []byte, ifMatch string) (json.RawMessage, error) {
	if operation == "meta" {
		return c.Do(ctx, http.MethodGet, "api/config/rules/meta", nil, "")
	}
	if operation == "list" {
		return c.Do(ctx, http.MethodGet, "api/config/rules/list", nil, "")
	}
	method := map[string]string{"get": http.MethodGet, "set": http.MethodPut, "patch": http.MethodPatch, "delete": http.MethodDelete, "reset": http.MethodPost}[operation]
	if method == "" {
		return nil, fmt.Errorf("unknown rule operation %q", operation)
	}
	return c.Do(ctx, method, "api/config/rules/"+operation+"/"+url.PathEscape(id), body, ifMatch)
}

func (c *Client) Action(ctx context.Context, name string, body []byte) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "api/action/"+strings.TrimLeft(name, "/"), body, "")
}

func ReadDocument(path, value string, stdin io.Reader) ([]byte, error) {
	var data []byte
	var err error
	switch {
	case path != "":
		data, err = os.ReadFile(path)
	case value != "":
		data = []byte(value)
	default:
		data, err = io.ReadAll(stdin)
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("input is empty")
	}
	if json.Valid(data) {
		return data, nil
	}
	var valueAny any
	if err := yaml.Unmarshal(data, &valueAny); err != nil {
		return nil, fmt.Errorf("parse input as JSON or YAML: %w", err)
	}
	valueAny = normalizeYAML(valueAny)
	return json.Marshal(valueAny)
}

func normalizeYAML(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = normalizeYAML(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[fmt.Sprint(key)] = normalizeYAML(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for index, item := range value {
			out[index] = normalizeYAML(item)
		}
		return out
	default:
		return value
	}
}

func Format(data json.RawMessage, output string) ([]byte, error) {
	if output == "json" || output == "" {
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return json.MarshalIndent(value, "", "  ")
	}
	if output == "yaml" {
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return yaml.Marshal(value)
	}
	if output == "raw" {
		return data, nil
	}
	return nil, fmt.Errorf("unsupported output format %q", output)
}
