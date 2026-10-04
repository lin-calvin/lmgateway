package lmgcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lmgcli.toml")
	if err := os.WriteFile(path, Template(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, timeout, err := ResolveConfig(cfg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server != "http://127.0.0.1:8080" || timeout.String() != "30s" {
		t.Fatalf("unexpected config: %+v timeout=%s", resolved, timeout)
	}
}

func TestResolveConfigEnvironmentOverridesFile(t *testing.T) {
	cfg, timeout, err := ResolveConfig(Config{
		Server:     "http://file",
		APIKeyEnv:  "FILE_KEY",
		AuthHeader: "X-API-Key",
		Output:     "yaml",
		Timeout:    "1s",
	}, func(name string) string {
		values := map[string]string{
			"LMGATEWAY_SERVER":   "http://env",
			"LMGCLI_API_KEY_ENV": "ENV_KEY",
			"LMGCLI_TIMEOUT":     "2s",
		}
		return values[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "http://env" || cfg.APIKeyEnv != "ENV_KEY" || timeout.String() != "2s" {
		t.Fatalf("environment did not override file: %+v timeout=%s", cfg, timeout)
	}
}
