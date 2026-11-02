package config

import (
	"chatbot-go/internal/crypto"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Line     LineConfig     `yaml:"line"`
	Weather  WeatherConfig  `yaml:"weather"`
	Alert    AlertConfig    `yaml:"alert"`
	AI       AIConfig       `yaml:"ai"`
	LIFF     LIFFConfig     `yaml:"liff"`
	Logging  LoggingConfig  `yaml:"logging"`
	Security SecurityConfig `yaml:"security"`
}

type SecurityConfig struct {
	// KeySet 儲存金鑰集合字串.
	KeySet     string `yaml:"keyset"`
	AdminToken string `yaml:"admin_token"`
}

const minProductionAdminTokenLength = 32

type ServerConfig struct {
	Port int `yaml:"port"`
	// TrustedProxies 前面的反向代理（逗號分隔的 CIDR）；設了才相信 X-Forwarded-For，否則用連線位址.
	TrustedProxies string `yaml:"trusted_proxies"`
}

type DatabaseConfig struct {
	Mongo MongoConfig `yaml:"mongo"`
	Redis RedisConfig `yaml:"redis"`
}

type MongoConfig struct {
	URI  string `yaml:"uri"`
	Name string `yaml:"name"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type LineConfig struct {
	ChannelSecret string `yaml:"channel_secret"`
	ChannelToken  string `yaml:"channel_token"`
	ReplyURL      string `yaml:"reply_url"`
	PushURL       string `yaml:"push_url"`
	// ContentURL 下載使用者傳來的圖片（後面接 /{messageId}/content）.
	ContentURL string `yaml:"content_url"`
	// LoadingURL 顯示「輸入中」動畫.
	LoadingURL string `yaml:"loading_url"`
}

// 預設 LINE API 位址，舊設定檔沒寫也能用.
const (
	defaultLineContentURL = "https://api-data.line.me/v2/bot/message"
	defaultLineLoadingURL = "https://api.line.me/v2/bot/chat/loading/start"
)

type AlertConfig struct {
	Cron                string `yaml:"cron"`
	WeatherDataset      string `yaml:"weather_dataset"`
	EarthquakeDataset   string `yaml:"earthquake_dataset"`
	TsunamiDataset      string `yaml:"tsunami_dataset"`
	CheckTimeoutSeconds int    `yaml:"check_timeout_seconds"`
}

// AIConfig AI 供應商設定；金鑰都沒設時 AI 功能自動停用，其他功能照常.
type AIConfig struct {
	// ProviderOrder 文字請求的供應商順序，逗號分隔；空白用預設（groq,gemini,nvidia）.
	ProviderOrder string `yaml:"provider_order"`
	// VisionProviderOrder 看圖請求的供應商順序；空白用預設（gemini,groq,nvidia）.
	VisionProviderOrder string `yaml:"vision_provider_order"`
	// QAProviderOrder 自由問答的供應商順序；空白用預設（claude,groq,gemini,nvidia）.
	QAProviderOrder string `yaml:"qa_provider_order"`
	GeminiAPIKey    string `yaml:"gemini_api_key"`
	GroqAPIKey      string `yaml:"groq_api_key"`
	NVIDIAAPIKey    string `yaml:"nvidia_api_key"`
	// AnthropicAPIKey Claude（付費）；只用在自由問答，其他功能維持免費供應商.
	AnthropicAPIKey string `yaml:"anthropic_api_key"`
	// ClaudeModel 空白用 ai.DefaultClaudeModel.
	ClaudeModel string `yaml:"claude_model"`
}

// defaultQAProviderOrder 自由問答預設先用 Claude，失敗再換免費供應商.
var defaultQAProviderOrder = []string{"claude", "groq", "gemini", "nvidia"}

// QAOrder 自由問答的供應商順序；回傳新的 slice，呼叫端修改不會影響預設值.
func (c *AIConfig) QAOrder() []string {
	if order := SplitList(c.QAProviderOrder); len(order) > 0 {
		return order
	}
	return slices.Clone(defaultQAProviderOrder)
}

// LIFFConfig LIFF 統計頁設定；兩個值都設了才啟用.
type LIFFConfig struct {
	// ID LIFF App ID（公開值，會寫進網頁）.
	ID string `yaml:"id"`
	// ChannelID LIFF 所屬 LINE Login channel 的 ID，用來驗證 ID token.
	ChannelID string `yaml:"channel_id"`
}

// Enabled 是否啟用 LIFF.
func (l *LIFFConfig) Enabled() bool { return l.ID != "" && l.ChannelID != "" }

// URL 開啟 LIFF 的網址.
func (l *LIFFConfig) URL() string { return "https://liff.line.me/" + l.ID }

// SplitList 把逗號分隔的設定拆成清單（去空白、轉小寫、略過空項）.
func SplitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type WeatherConfig struct {
	Cron            string `yaml:"cron"`
	CWABaseURL      string `yaml:"cwa_base_url"`
	CWAAuthKey      string `yaml:"cwa_auth_key"`
	RateLimitMs     int    `yaml:"rate_limit_ms"`
	CacheTTLSeconds int    `yaml:"cache_ttl_seconds"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

var cfg *Config

func Load() error {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "local"
	}

	path := filepath.Clean(fmt.Sprintf("./configs/%s.yaml", env))
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// 先解析 YAML、再逐欄替換 ${VAR}：以前是整份文字先替換再解析，
	// 密碼裡的「#」會被當成註解截斷、「$」會被吃掉、「: 」會讓解析失敗.
	if unset := expandEnvFields(reflect.ValueOf(c).Elem()); len(unset) > 0 {
		slog.Warn("config references unset environment variables", "names", strings.Join(unset, ","))
	}

	// 1. 初始化金鑰庫.
	if err := initVault(c); err != nil {
		return fmt.Errorf("vault init fail: %w", err)
	}

	// 2. 自動解密 ENC(...) 格式欄位.
	if err := decryptConfig(c); err != nil {
		return fmt.Errorf("decrypt config: %w", err)
	}

	applyDefaults(c)

	if err := validate(c, env); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}

	cfg = c
	return nil
}

func Get() *Config {
	return cfg
}

// Set 直接指定設定（測試用）.
func Set(c *Config) {
	cfg = c
}

func applyDefaults(c *Config) {
	if c.Line.ContentURL == "" {
		c.Line.ContentURL = defaultLineContentURL
	}
	if c.Line.LoadingURL == "" {
		c.Line.LoadingURL = defaultLineLoadingURL
	}
}

func validate(c *Config, env string) error {
	var missing []string
	if c.Database.Mongo.URI == "" {
		missing = append(missing, "database.mongo.uri")
	}
	if c.Line.ChannelSecret == "" {
		missing = append(missing, "line.channel_secret")
	}
	if c.Line.ChannelToken == "" {
		missing = append(missing, "line.channel_token")
	}
	if env == "production" {
		if c.Security.AdminToken == "" {
			missing = append(missing, "security.admin_token")
		} else if len(c.Security.AdminToken) < minProductionAdminTokenLength {
			missing = append(missing, "security.admin_token length >= 32")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// initVault 驗證 Tink keyset.
func initVault(c *Config) error {
	if c.Security.KeySet == "" {
		return nil
	}
	if err := crypto.ValidateKeySet(c.Security.KeySet); err != nil {
		return fmt.Errorf("invalid keyset: %w", err)
	}
	return nil
}

// expandEnvFields 把每個字串欄位裡的 ${VAR} 換成環境變數；回傳有引用但沒設定的變數名稱（不含值）.
func expandEnvFields(v reflect.Value) []string {
	seen := map[string]bool{}
	var unset []string
	lookup := func(name string) string {
		val, ok := os.LookupEnv(name)
		if !ok && !seen[name] {
			seen[name] = true
			unset = append(unset, name)
		}
		return val
	}
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Ptr:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				walk(v.Field(i))
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.String:
			if v.CanSet() && strings.Contains(v.String(), "$") {
				v.SetString(os.Expand(v.String(), lookup))
			}
		}
	}
	walk(v)
	sort.Strings(unset)
	return unset
}

func decryptConfig(c *Config) error {
	v := reflect.ValueOf(c)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	return walkAndDecrypt(v, c.Security.KeySet)
}

func walkAndDecrypt(v reflect.Value, keyset string) error {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}
		return walkAndDecrypt(v.Elem(), keyset)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Name == "Security" {
				continue
			}
			if err := walkAndDecrypt(v.Field(i), keyset); err != nil {
				return err
			}
		}
	case reflect.String:
		if !v.CanSet() {
			return nil
		}
		val := v.String()
		if strings.HasPrefix(val, "ENC(") && strings.HasSuffix(val, ")") {
			content := strings.TrimSuffix(strings.TrimPrefix(val, "ENC("), ")")
			cipherText, err := parseENC(content)
			if err != nil {
				return err
			}

			plain, err := crypto.Decrypt(cipherText, keyset)
			if err != nil {
				return fmt.Errorf("failed to decrypt field: %w", err)
			}

			v.SetString(plain)
		}
	}
	return nil
}

func parseENC(content string) (cipherText string, err error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("invalid ENC content, payload is empty")
	}
	return content, nil
}
