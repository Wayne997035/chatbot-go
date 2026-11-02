package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestQAOrder(t *testing.T) {
	if got := strings.Join((&AIConfig{}).QAOrder(), ","); got != "claude,groq,gemini,nvidia" {
		t.Errorf("default = %s", got)
	}
	if got := strings.Join((&AIConfig{QAProviderOrder: " Groq, claude ,,"}).QAOrder(), ","); got != "groq,claude" {
		t.Errorf("custom = %s", got)
	}
	order := (&AIConfig{}).QAOrder()
	order[0] = "changed"
	if (&AIConfig{}).QAOrder()[0] != "claude" {
		t.Error("callers must not be able to change the default order")
	}
}

// 環境變數的值原封不動進到設定：以前整份 YAML 先替換再解析，「#」之後被當成註解、「$」被吃掉、「: 」讓解析失敗.
func TestLoadKeepsSpecialCharactersInEnvValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o750); err != nil {
		t.Fatal(err)
	}
	yamlText := `server:
  port: 8080
database:
  mongo:
    uri: ${MONGO_URI}
  redis:
    addr: ${REDIS_ADDR}
    password: ${REDIS_PASSWORD}
line:
  channel_secret: ${LINE_CHANNEL_SECRET}
  channel_token: ${LINE_CHANNEL_TOKEN}
security:
  admin_token: ${ADMIN_TOKEN}
`
	if err := os.WriteFile(filepath.Join(dir, "configs", "testenv.yaml"), []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("APP_ENV", "testenv")
	t.Setenv("MONGO_URI", "mongodb://u:p@h:27017/?a=1&b=2")
	t.Setenv("REDIS_PASSWORD", "abc #123 $HOME: !tag *x {y")
	t.Setenv("LINE_CHANNEL_SECRET", "s$ecret")
	t.Setenv("LINE_CHANNEL_TOKEN", "token: with colon")
	t.Setenv("ADMIN_TOKEN", "lit$abc$$x")
	t.Setenv("REDIS_ADDR", "") // 先登記，測試結束會還原
	if err := os.Unsetenv("REDIS_ADDR"); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checks := map[string][2]string{
		"mongo uri":      {c.Database.Mongo.URI, "mongodb://u:p@h:27017/?a=1&b=2"},
		"redis password": {c.Database.Redis.Password, "abc #123 $HOME: !tag *x {y"},
		"line secret":    {c.Line.ChannelSecret, "s$ecret"},
		"line token":     {c.Line.ChannelToken, "token: with colon"},
		"admin token":    {c.Security.AdminToken, "lit$abc$$x"},
		"unset addr":     {c.Database.Redis.Addr, ""},
	}
	for name, got := range checks {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
}

func TestExpandEnvFieldsReportsUnsetNames(t *testing.T) {
	t.Setenv("SET_ONE", "x")
	t.Setenv("NOT_SET_A", "")
	if err := os.Unsetenv("NOT_SET_A"); err != nil {
		t.Fatal(err)
	}
	c := &Config{Server: ServerConfig{TrustedProxies: "${NOT_SET_A}"}, Weather: WeatherConfig{Cron: "${SET_ONE}"}}
	unset := expandEnvFields(reflect.ValueOf(c).Elem())
	if len(unset) != 1 || unset[0] != "NOT_SET_A" || c.Weather.Cron != "x" || c.Server.TrustedProxies != "" {
		t.Errorf("unset=%v cron=%q proxies=%q", unset, c.Weather.Cron, c.Server.TrustedProxies)
	}
}
