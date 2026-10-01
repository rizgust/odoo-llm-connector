package config

import (
	"slices"
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Setenv("ODOO_URL", "https://odoo.example.com/")
	t.Setenv("ODOO_DB", "prod")
	t.Setenv("ODOO_USER", "bot")
	t.Setenv("ODOO_API_KEY", "key")
	t.Setenv("MCP_ACCESS_TOKEN", strings.Repeat("a", 32))
}

func TestFromEnvDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OdooURL != "https://odoo.example.com" || cfg.ListenAddr != ":8000" || cfg.MaxLimit != 500 {
		t.Errorf("cfg = %+v", cfg)
	}
	if !slices.Equal(cfg.BlockedModels, DefaultBlockedModels) {
		t.Errorf("blocked = %v", cfg.BlockedModels)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("ODOO_ALLOWED_MODELS", "sale.*, account.move ,")
	t.Setenv("ODOO_BLOCKED_MODELS", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.AllowedModels, []string{"sale.*", "account.move"}) || len(cfg.BlockedModels) != 0 {
		t.Errorf("allowed = %v blocked = %v", cfg.AllowedModels, cfg.BlockedModels)
	}
}

func TestFromEnvErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"missing":     {"ODOO_DB": ""},
		"short token": {"MCP_ACCESS_TOKEN": "short"},
		"bad token":   {"MCP_ACCESS_TOKEN": strings.Repeat("a", 31) + "/"},
		"bad limit":   {"ODOO_MAX_LIMIT": "lots"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := FromEnv(); err == nil {
				t.Error("expected error")
			}
		})
	}
}
