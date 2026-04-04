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
	Crypto   CryptoConfig   `yaml:"crypto"`
}

type CryptoConfig struct {
	ActiveKeyID string            `yaml:"active_key_id"`
	Keys        map[string]string `yaml:"keys"`
}

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

	if err := decryptConfig(c); err != nil {
		return fmt.Errorf("decrypt config: %w", err)
	}

	if err := validate(c); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}

	cfg = c
	return nil
}

func Get() *Config {
	return cfg
}

func validate(c *Config) error {
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
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

func decryptConfig(c *Config) error {
	if len(c.Crypto.Keys) == 0 {
		return nil
	}

	v := reflect.ValueOf(c)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	return walkAndDecrypt(v, c.Crypto.Keys)
}

func walkAndDecrypt(v reflect.Value, keys map[string]string) error {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}
		return walkAndDecrypt(v.Elem(), keys)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Name == "Crypto" {
				continue
			}
			if err := walkAndDecrypt(v.Field(i), keys); err != nil {
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
			parts := strings.SplitN(content, ":", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid ENC format, expected ENC(key_id:ciphertext)")
			}

			keyID, cipherText := parts[0], parts[1]
			masterKey, ok := keys[keyID]
			if !ok {
				return fmt.Errorf("crypto key id %q not found in config", keyID)
			}

			plainText, err := crypto.DecryptAESGCM(cipherText, masterKey)
			if err != nil {
				return fmt.Errorf("decryption failed for key %q: %w", keyID, err)
			}
			v.SetString(plainText)
		}
	}
	return nil
}
