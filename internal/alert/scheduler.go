package alert

import (
	"chatbot-go/internal/platform/config"
	"context"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"
)

// Scheduler 警報排程器.
type Scheduler struct {
	scheduler gocron.Scheduler
	checker   *Checker
	cfg       *config.Config
}

// NewScheduler 建立警報排程器.
func NewScheduler(checker *Checker, cfg *config.Config) (*Scheduler, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}

	return &Scheduler{
		scheduler: s,
		checker:   checker,
		cfg:       cfg,
	}, nil
}

// Start 啟動排程：定時 + 啟動時立即執行一次.
func (s *Scheduler) Start() error {
	_, err := s.scheduler.NewJob(
		gocron.CronJob(s.cfg.Alert.Cron, false),
		gocron.NewTask(s.runCheck),
		gocron.WithName("alert-check"),
	)
	if err != nil {
		return err
	}

	s.scheduler.Start()
	slog.Info("alert scheduler started", "cron", s.cfg.Alert.Cron)

	// 啟動後非同步執行一次檢查
	go func() {
		slog.Info("alert initial check started")
		s.runCheck()
		slog.Info("alert initial check completed")
	}()

	return nil
}

// Stop 停止排程器.
func (s *Scheduler) Stop() error {
	return s.scheduler.Shutdown()
}

func (s *Scheduler) runCheck() {
	timeout := s.cfg.Alert.CheckTimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	if err := s.checker.CheckAndNotify(ctx); err != nil {
		slog.Error("alert check failed", "error", err)
		return
	}

	slog.Info("alert check completed")
}
