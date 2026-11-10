package alert

import (
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"
)

// Scheduler 警報排程器.
type Scheduler struct {
	scheduler gocron.Scheduler
	checker   *Checker
	cfg       *config.Config
	work      *background.Group // 啟動時的檢查與排程中的檢查；關機時等它們推播完
}

// NewScheduler 建立警報排程器.
func NewScheduler(checker *Checker, cfg *config.Config) (*Scheduler, error) {
	s, err := gocron.NewScheduler(gocron.WithStopTimeout(background.SchedulerStopTimeout)) // [SEC29-05][F293-02] 關機預算算進它
	if err != nil {
		return nil, err
	}

	return &Scheduler{
		scheduler: s,
		checker:   checker,
		cfg:       cfg,
		work:      background.New(0, 0),
	}, nil
}

// Start 啟動排程：定時 + 啟動時立即執行一次.
func (s *Scheduler) Start() error {
	_, err := s.scheduler.NewJob(
		gocron.CronJob(s.cfg.Alert.Cron, false),
		gocron.NewTask(func() { s.work.Do(s.runCheck) }),
		gocron.WithName("alert-check"),
	)
	if err != nil {
		return err
	}

	s.scheduler.Start()
	slog.Info("alert scheduler started", "cron", s.cfg.Alert.Cron)

	// 啟動後非同步執行一次檢查
	s.work.Go(func(ctx context.Context) {
		slog.Info("alert initial check started")
		s.runCheck(ctx)
	})

	return nil
}

// Stop 停止排程，等進行中的檢查推播完（推到一半中斷的人重啟後會補送）；ctx 到期時取消.
func (s *Scheduler) Stop(ctx context.Context) error {
	return errors.Join(s.work.Shutdown(ctx), s.scheduler.Shutdown())
}

func (s *Scheduler) runCheck(ctx context.Context) {
	timeout := s.cfg.Alert.CheckTimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	if err := s.checker.CheckAndNotify(ctx); err != nil {
		slog.Error("alert check failed", "error", err)
		return
	}

	// [F24-08] 正常完成不印 Info（正式環境每 5 分鐘一行雜訊）；失敗上面已印 Error
	slog.Debug("alert check completed")
}
