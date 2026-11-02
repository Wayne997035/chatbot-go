package weather

import (
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
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
	work        *background.Group // 啟動時的同步與排程中的同步
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
		work:        background.New(0, 0),
	}, nil
}

// Start 啟動排程：定時 + 啟動時立即執行一次.
func (s *Scheduler) Start() error {
	_, err := s.scheduler.NewJob(
		gocron.CronJob(s.cfg.Weather.Cron, false),
		gocron.NewTask(func() { s.work.Do(s.syncWeather) }),
		gocron.WithName("weather-sync"),
	)
	if err != nil {
		return err
	}

	s.scheduler.Start()
	slog.Info("weather scheduler started", "cron", s.cfg.Weather.Cron)

	// 啟動後非同步預載一次天氣資料
	s.work.Go(func(ctx context.Context) {
		slog.Info("weather initial sync started")
		s.syncWeather(ctx)
	})

	return nil
}

// Stop 停止排程並直接取消進行中的同步（同步可能要好幾分鐘，資料下次啟動會再抓），等它收尾.
func (s *Scheduler) Stop(ctx context.Context) error {
	return errors.Join(stopNow(ctx, s.work), s.scheduler.Shutdown())
}

// stopNow 不等工作做完，立即取消並等它們收尾.
func stopNow(ctx context.Context, g *background.Group) error {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := g.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (s *Scheduler) syncWeather(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
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
