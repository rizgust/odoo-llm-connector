// Package config loads settings from environment variables.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBlockedModels are technical or credential-bearing models nobody needs for
// business reporting. Patterns use path.Match syntax; override with ODOO_BLOCKED_MODELS.
var DefaultBlockedModels = []string{
	"ir.*",
	"base.*",
	"bus.*",
	"iap.*",
	"auth.*",
	"res.users.apikeys*",
	"res.users.log",
	"res.config*",
	"res.partner.bank",
	"fetchmail.*",
	"payment.token",
	"payment.provider",
	"mail.mail",
	"mail.alias*",
}

var tokenChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Config struct {
	OdooURL     string
	OdooDB      string
	OdooUser    string
	OdooAPIKey  string
	OdooTimeout time.Duration

	// AccessToken is the secret path segment of the MCP endpoint: /mcp/<AccessToken>.
	AccessToken string
	ListenAddr  string
	// PublicHost, when set, is the only Host header accepted on the MCP endpoint.
	PublicHost string

	AllowedModels []string // empty = everything not blocked
	BlockedModels []string

	DefaultLimit  int
	MaxLimit      int
	MaxTextLength int
}

func FromEnv() (*Config, error) {
	var missing []string
	need := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := &Config{
		OdooURL:     strings.TrimRight(need("ODOO_URL"), "/"),
		OdooDB:      need("ODOO_DB"),
		OdooUser:    need("ODOO_USER"),
		OdooAPIKey:  need("ODOO_API_KEY"),
		AccessToken: need("MCP_ACCESS_TOKEN"),
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if len(cfg.AccessToken) < 32 {
		return nil, fmt.Errorf("MCP_ACCESS_TOKEN must be at least 32 characters (it is the only thing guarding the URL)")
	}
	if !tokenChars.MatchString(cfg.AccessToken) {
		return nil, fmt.Errorf("MCP_ACCESS_TOKEN may only contain letters, digits, '-' and '_' (it is part of the URL path)")
	}

	cfg.ListenAddr = envOr("MCP_LISTEN_ADDR", ":8000")
	cfg.PublicHost = os.Getenv("MCP_PUBLIC_HOST")
	cfg.AllowedModels = csv(os.Getenv("ODOO_ALLOWED_MODELS"))
	cfg.BlockedModels = DefaultBlockedModels
	if v, ok := os.LookupEnv("ODOO_BLOCKED_MODELS"); ok {
		cfg.BlockedModels = csv(v)
	}

	var err error
	if cfg.DefaultLimit, err = envInt("ODOO_DEFAULT_LIMIT", 80); err != nil {
		return nil, err
	}
	if cfg.MaxLimit, err = envInt("ODOO_MAX_LIMIT", 500); err != nil {
		return nil, err
	}
	if cfg.MaxTextLength, err = envInt("ODOO_MAX_TEXT_LENGTH", 500); err != nil {
		return nil, err
	}
	timeout, err := envInt("ODOO_TIMEOUT_SECONDS", 60)
	if err != nil {
		return nil, err
	}
	cfg.OdooTimeout = time.Duration(timeout) * time.Second
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

func csv(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
