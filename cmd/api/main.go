package main

import (
	"chatbot-go/internal/app"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/logger"
	"context"
	"log"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.File); err != nil {
		return err
	}
	defer logger.Close()

	return app.Run(context.Background(), cfg)
}
