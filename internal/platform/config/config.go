package config

import (
	"chatbot-go/internal/crypto"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Line     LineConfig     `yaml:"line"`
	Weather  WeatherConfig  `yaml:"weather"`
	Alert    AlertConfig    `yaml:"alert"`
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
}

type AlertConfig struct {
	Cron                string `yaml:"cron"`
	WeatherDataset      string `yaml:"weather_dataset"`
	EarthquakeDataset   string `yaml:"earthquake_dataset"`
	TsunamiDataset      string `yaml:"tsunami_dataset"`
	CheckTimeoutSeconds int    `yaml:"check_timeout_seconds"`
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

	expanded := os.ExpandEnv(string(data))

	c := &Config{}
	if err := yaml.Unmarshal([]byte(expanded), c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// 1. 初始化金鑰庫.
	if err := initVault(c); err != nil {
		return fmt.Errorf("vault init fail: %w", err)
	}

	// 2. 自動解密 ENC(...) 格式欄位.
	if err := decryptConfig(c); err != nil {
		return fmt.Errorf("decrypt config: %w", err)
	}

	if err := validate(c, env); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}

	cfg = c
	return nil
}

func Get() *Config {
	return cfg
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
