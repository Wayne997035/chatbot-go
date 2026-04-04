package main

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/driver"
	"chatbot-go/internal/platform/logger"
	"chatbot-go/internal/platform/server"
	"chatbot-go/internal/storage/database"
	"chatbot-go/internal/weather"
	"log"
	"log/slog"
)

func main() {
	if err := mainNoExit(); err != nil {
		log.Fatal(err)
	}
}

func mainNoExit() error {
	// 1. Config
	if err := config.Load(); err != nil {
		return err
	}
	cfg := config.Get()

	// 2. Logger (slog + lumberjack)
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.File); err != nil {
		return err
	}
	defer logger.Close()

	// 3. MongoDB
	if err := driver.ConnectMongo(cfg); err != nil {
		return err
	}
	defer driver.CloseMongo()

	// 4. Redis（連不上不影響啟動，降級為無快取）
	if err := driver.ConnectRedis(cfg); err != nil {
		slog.Warn("Redis unavailable, running without cache", "error", err)
	} else {
		defer driver.CloseRedis()
	}

	// 5. DI（手動 constructor injection）
	repos := database.NewRepositories()

	// 6. Weather scheduler
	weatherScheduler, err := weather.NewScheduler(repos.Weather, cfg)
	if err != nil {
		return err
	}
	if cfg.Weather.CWAAuthKey != "" {
		if err := weatherScheduler.Start(); err != nil {
			slog.Error("weather scheduler start failed", "error", err)
		}
	} else {
		slog.Warn("CWA auth key not set, weather scheduler disabled")
	}

	// 7. HTTP Server（blocking）
	return server.Start(repos, weatherScheduler, cfg)
}
