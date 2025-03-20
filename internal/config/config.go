package config

import (
	"os"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const DefaultConfigPath = "./configs/config.yaml"

type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Logger   *zap.Logger
}

type DatabaseConfig struct {
	Mongo MongoConfig `yaml:"mongo"`
}

type MongoConfig struct {
	URL      string `yaml:"url"`
	Name     string `yaml:"name"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
}

func NewLogger() *zap.Logger {
	logger, err := zap.NewProduction()
	if err != nil {
		os.Exit(1)
	}

	return logger
}

func NewConfig(logger *zap.Logger) *Config {

	config := &Config{}

	data, err := os.ReadFile(DefaultConfigPath)
	if err != nil {
		logger.Fatal("failed to read config file", zap.Error(err))
		return nil
	}

	if err = yaml.Unmarshal(data, config); err != nil {
		logger.Fatal("failed to unmarshal config file", zap.Error(err))
		return nil
	}

	return config
}
