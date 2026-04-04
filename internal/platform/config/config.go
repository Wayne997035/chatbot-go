package config

import (
	"fmt"
	"os"
	"path/filepath"
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

// AlertConfig 災害警報推播設定.
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
