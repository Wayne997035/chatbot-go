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
	CWA      CWAConfig      `yaml:"cwa"`
}

type SecurityConfig struct {
	// KeySet 儲存金鑰集合字串.
	KeySet     string `yaml:"keyset"`
	AdminToken string `yaml:"admin_token"`
	// RequireEncrypted 強制模式開關（env REQUIRE_ENCRYPTED_SECRETS）；必須是字串：`${...}` 在 Unmarshal 時不是合法 bool.
	RequireEncrypted string `yaml:"require_encrypted"`
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
	WeatherDataset      string `yaml:"weather_dataset"`
	EarthquakeDataset   string `yaml:"earthquake_dataset"`
	TsunamiDataset      string `yaml:"tsunami_dataset"`
	CheckTimeoutSeconds int    `yaml:"check_timeout_seconds"`
	// 以下為氣象署排程頻率與地震筆數（F30-36）；零值 / 空字串 = 程式碼預設，字面 off = 停用該排程.
	WarningCron         string `yaml:"warning_cron"`
	EarthquakeCron      string `yaml:"earthquake_cron"`
	TsunamiCron         string `yaml:"tsunami_cron"`
	ColdIntervalMinutes int    `yaml:"cold_interval_minutes"`
	EarthquakeLimit     int    `yaml:"earthquake_limit"`
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
	// [F33-10] 以下七個一律是字串（環境變數只展開字串欄位）；空 = 未設，用 ai 套件預設. 驗證見 sanitizeAI.
	// NVIDIAModels / GroqModels / GeminiModels 指定模型清單（逗號分隔、保留大小寫），空 = 自動排名.
	NVIDIAModels string `yaml:"nvidia_models"`
	GroqModels   string `yaml:"groq_models"`
	GeminiModels string `yaml:"gemini_models"`
	// 單位皆為整數秒.
	AttemptTimeoutSeconds   string `yaml:"attempt_timeout_seconds"`
	ProviderBudgetSeconds   string `yaml:"provider_budget_seconds"`
	ProviderCooldownSeconds string `yaml:"provider_cooldown_seconds"`
	RateLimitWaitSeconds    string `yaml:"rate_limit_wait_seconds"`
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

// Load 依 APP_ENV 讀取設定檔（configs/<APP_ENV>.yaml，預設 local）並驗證. 沒有全域狀態：呼叫端把結果傳給需要的元件.
func Load() (*Config, error) {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "local"
	}

	path := filepath.Clean(fmt.Sprintf("./configs/%s.yaml", env))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 先解析 YAML、再逐欄替換 ${VAR}：以前是整份文字先替換再解析，
	// 密碼裡的「#」會被當成註解截斷、「$」會被吃掉、「: 」會讓解析失敗.
	if unset := expandEnvFields(reflect.ValueOf(c).Elem()); len(unset) > 0 {
		slog.Warn("config references unset environment variables", "names", strings.Join(unset, ","))
	}

	// [F34-37] keyset 本身不能是 ENC；一定要在 initVault 之前，否則 ENC 字串先進 ValidateKeySet 只會得到 base64/格式錯誤.
	if crypto.IsENC(strings.TrimSpace(c.Security.KeySet)) {
		return nil, fmt.Errorf("security.keyset must not be ENC")
	}

	// 1. 初始化金鑰庫（`KMS(` 開頭在這裡 fail-closed：警告與強制模式皆然）.
	if err := initVault(c); err != nil {
		return nil, fmt.Errorf("vault init fail: %w", err)
	}

	// 2. 自動解密 ENC(...) 格式欄位.
	st, err := decryptConfig(c)
	if err != nil {
		return nil, fmt.Errorf("decrypt config: %w", err)
	}
	// [F34-38] production：明文警告/強制、非主金鑰警告.
	if err := checkSecrets(c, env, st); err != nil {
		return nil, fmt.Errorf("secrets check: %w", err)
	}

	applyDefaults(c)
	// 氣象署設定：環境變數覆蓋 + 逐欄驗證；無效欄位退回預設並 Warn，不讓 Load 失敗（F30-36）.
	applyCWASettings(c)
	// [F33-08] AI 路由設定：非法值清空並 Warn（不含值），不讓 Load 失敗.
	sanitizeAI(c)

	if err := validate(c, env); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return c, nil
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
	if strings.TrimSpace(c.Security.KeySet) == "" {
		return nil
	}
	if err := crypto.ValidateKeySet(c.Security.KeySet); err != nil {
		return fmt.Errorf("%s: %w", keysetPath, err)
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

func decryptConfig(c *Config) (*decryptState, error) {
	st := &decryptState{keyset: c.Security.KeySet, encrypted: map[string]string{}}
	if err := walkAndDecrypt(reflect.ValueOf(c).Elem(), "", st); err != nil {
		return nil, err
	}
	return st, nil
}

// walkAndDecrypt [F34-37] 帶 yaml 路徑往下走，路徑就是 ENC 的 AAD；只跳過 security.keyset（不再跳過整個 Security）.
func walkAndDecrypt(v reflect.Value, path string, st *decryptState) error {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}
		return walkAndDecrypt(v.Elem(), path, st)
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			name := yamlName(&field)
			if name == "" {
				continue
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
			if child == keysetPath {
				continue
			}
			if err := walkAndDecrypt(v.Field(i), child, st); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.CanSet() {
			return decryptString(v, path, st)
		}
	}
	return nil
}

// decryptString 解開單一欄位；錯誤只帶欄位路徑與原因類別，不帶值、不帶密文.
func decryptString(v reflect.Value, path string, st *decryptState) error {
	// [F34-37] 貼值常多帶換行或空白：先去頭尾，否則整串 ENC 會被原樣當明文用.
	val := strings.TrimSpace(v.String())
	if !crypto.IsENC(val) {
		// 開頭是 ENC( 卻不成形(截斷),或含 ENC(v1: 卻不是整串(貼了整行 NAME=ENC(…)、加了引號)→ 啟動失敗,不當明文用.
		// 只認 ENC(v1: 這個完整前綴,不認任意位置的 ENC(:明文密碼剛好含這 7 個字元的機率遠低於含 ENC(.
		if strings.HasPrefix(val, "ENC(") || strings.Contains(val, "ENC(v1:") {
			return fmt.Errorf("%s: malformed ENC value", path)
		}
		return nil
	}
	if strings.TrimSpace(st.keyset) == "" {
		return fmt.Errorf("%s: no keyset", path)
	}
	plain, legacy, err := crypto.DecryptValue(val, st.keyset, path)
	if err != nil {
		return fmt.Errorf("decrypt %s: %w", path, err)
	}
	if legacy {
		slog.Warn("config field uses legacy ENC format, run cmd/secrets reencrypt", "field", path)
	}
	st.encrypted[path] = val
	v.SetString(plain)
	return nil
}
