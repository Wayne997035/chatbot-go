package config

import (
	"strings"
	"testing"
)

func TestValidateRequiresStrongProductionAdminToken(t *testing.T) {
	cfg := validTestConfig()
	cfg.Security.AdminToken = ""

	err := validate(cfg, "production")
	if err == nil || !strings.Contains(err.Error(), "security.admin_token") {
		t.Fatalf("validate() error = %v, want missing admin token", err)
	}

	cfg.Security.AdminToken = strings.Repeat("a", minProductionAdminTokenLength-1)
	err = validate(cfg, "production")
	if err == nil || !strings.Contains(err.Error(), "security.admin_token length >= 32") {
		t.Fatalf("validate() error = %v, want weak admin token", err)
	}

	cfg.Security.AdminToken = strings.Repeat("a", minProductionAdminTokenLength)
	if err := validate(cfg, "production"); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestValidateAllowsMissingLocalAdminToken(t *testing.T) {
	cfg := validTestConfig()
	cfg.Security.AdminToken = ""

	if err := validate(cfg, "local"); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func validTestConfig() *Config {
	return &Config{
		Database: DatabaseConfig{
			Mongo: MongoConfig{URI: "mongodb://localhost:27017", Name: "chatbot"},
		},
		Line: LineConfig{
			ChannelSecret: "line-secret",
			ChannelToken:  "line-token",
		},
		Security: SecurityConfig{
			AdminToken: strings.Repeat("a", minProductionAdminTokenLength),
		},
	}
}
