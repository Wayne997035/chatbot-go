package config

import (
	"os"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const LineMessageConfigPath = "./configs/lineMessage.yaml"

type LineConfig struct {
	Line   MessageConfig `yaml:"line"`
	Logger *zap.Logger
}

type MessageConfig struct {
	Message LineMessageConfig `yaml:"message"`
}

type LineMessageConfig struct {
	LineReplyURL  string `yaml:"lineReplyURL"`
	ChannelToken  string `yaml:"channelToken"`
	ChannelSecret string `yaml:"channelSecret"`
}

func NewLineConfig(logger *zap.Logger) *LineConfig {

	config := &LineConfig{}

	data, err := os.ReadFile(LineMessageConfigPath)
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
