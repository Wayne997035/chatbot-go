package weather

import (
	"chatbot-go/internal/platform/config"
	"context"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// Scheduler 天氣資料排程同步器.
type Scheduler struct {
	scheduler   gocron.Scheduler
	weatherRepo weatherstore.WeatherRepository
	cfg         *config.Config
}

// NewScheduler 建立排程器.
func NewScheduler(weatherRepo weatherstore.WeatherRepository, cfg *config.Config) (*Scheduler, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}

	return &Scheduler{
		scheduler:   s,
		weatherRepo: weatherRepo,
		cfg:         cfg,
	}, nil
}

// Start 啟動排程：定時 + 啟動時立即執行一次.
func (s *Scheduler) Start() error {
	_, err := s.scheduler.NewJob(
		gocron.CronJob(s.cfg.Weather.Cron, false),
		gocron.NewTask(s.syncWeather),
		gocron.WithName("weather-sync"),
	)
	if err != nil {
		return err
	}

	s.scheduler.Start()
	slog.Info("weather scheduler started", "cron", s.cfg.Weather.Cron)

	// 啟動後非同步預載一次天氣資料
	go func() {
		slog.Info("weather initial sync started")
		s.syncWeather()
		slog.Info("weather initial sync completed")
	}()

	return nil
}

// Stop 停止排程器.
func (s *Scheduler) Stop() error {
	return s.scheduler.Shutdown()
}

func (s *Scheduler) syncWeather() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := FetchAndStore(
		ctx,
		s.cfg.Weather.CWABaseURL,
		s.cfg.Weather.CWAAuthKey,
		s.cfg.Weather.RateLimitMs,
		s.weatherRepo,
	); err != nil {
		slog.Error("weather sync failed", "error", err)
		return
	}

	slog.Info("weather sync completed")
}
