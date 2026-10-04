package lmgcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     string `toml:"server"`
	APIKeyEnv  string `toml:"api_key_env"`
	APIKeyFile string `toml:"api_key_file"`
	AuthHeader string `toml:"auth_header"`
	Output     string `toml:"output"`
	Timeout    string `toml:"timeout"`
}

func DefaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "lmgcli.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "lmgcli.toml"
	}
	return filepath.Join(home, ".config", "lmgcli.toml")
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	if path == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func ResolveConfig(file Config, env func(string) string) (Config, time.Duration, error) {
	cfg := file
	if value := env("LMGATEWAY_SERVER"); value != "" {
		cfg.Server = value
	}
	if value := env("LMGCLI_API_KEY_ENV"); value != "" {
		cfg.APIKeyEnv = value
	}
	if value := env("LMGCLI_API_KEY_FILE"); value != "" {
		cfg.APIKeyFile = value
	}
	if value := env("LMGCLI_AUTH_HEADER"); value != "" {
		cfg.AuthHeader = value
	}
	if value := env("LMGCLI_OUTPUT"); value != "" {
		cfg.Output = value
	}
	if value := env("LMGCLI_TIMEOUT"); value != "" {
		cfg.Timeout = value
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	if cfg.AuthHeader == "" {
		cfg.AuthHeader = "Authorization"
	}
	if cfg.Output == "" {
		cfg.Output = "json"
	}
	timeout := 30 * time.Second
	if cfg.Timeout != "" {
		parsed, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return Config{}, 0, fmt.Errorf("invalid timeout %q: %w", cfg.Timeout, err)
		}
		timeout = parsed
	}
	return cfg, timeout, nil
}

func ReadAPIKey(cfg Config, env func(string) string) (string, error) {
	if value := env("LMGATEWAY_API_KEY"); value != "" {
		return value, nil
	}
	if cfg.APIKeyEnv != "" {
		return env(cfg.APIKeyEnv), nil
	}
	if cfg.APIKeyFile != "" {
		data, err := os.ReadFile(ExpandPath(cfg.APIKeyFile))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", nil
}

func Template() []byte {
	return []byte("server = \"http://127.0.0.1:8080\"\nauth_header = \"Authorization\"\noutput = \"json\"\ntimeout = \"30s\"\n# api_key_env = \"LMGATEWAY_API_KEY\"\n# api_key_file = \"~/.config/lmgcli/api.key\"\n")
}

func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func ParseBool(value string) (bool, bool) {
	parsed, err := strconv.ParseBool(value)
	return parsed, err == nil
}
