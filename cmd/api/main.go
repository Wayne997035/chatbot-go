package main

import (
	"chatbot-go/internal/app"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/logger"
	"context"
	"os"
)

func main() {
	if err := run(); err != nil {
		// [SEC-R3-02] log.Fatal 經 slog 只是 INFO；開機致命錯誤要 ERROR 並遮蔽.
		logger.ReportFatal(err)
		os.Exit(1)
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
