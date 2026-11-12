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

// 三個排程的 job 名稱（F30-41）.
const (
	jobWarning    = "cwa-warning"
	jobEarthquake = "cwa-earthquake"
	jobTsunami    = "cwa-tsunami"
)

// 每個排程一輪最多會打幾次氣象署（期限推導用）：特報 = 低溫 + 001 + 兩份鄉鎮明細，地震 = 地震 + 觸發的海嘯.
const (
	warningFetches    = 4
	earthquakeFetches = 2
	tsunamiFetches    = 1
	// jobSlack 推播與其他處理的餘裕（推播耗時未量測）.
	jobSlack = 30 * time.Second
	// defaultCheckTimeout check_timeout_seconds 未設（≤ 0）時的下限.
	defaultCheckTimeout = 120 * time.Second
)

// Scheduler 警報排程器：特報、地震、海嘯三個排程各自設定頻率.
type Scheduler struct {
	scheduler gocron.Scheduler // loc 為 nil 時不建立
	checker   *Checker
	cfg       *config.Config
	work      *background.Group // 啟動時的檢查與排程中的檢查；關機時等它們推播完
	enabled   bool              // false = 時區載入失敗，Start 不註冊任何 job
}

// NewScheduler 建立警報排程器. loc 為台灣時區；nil（載入失敗）時 Start 不註冊任何 job、不跑啟動檢查
// （失敗的 Error 由 app 印一次，本套件不重印）.
func NewScheduler(checker *Checker, cfg *config.Config, loc *time.Location) (*Scheduler, error) {
	s := &Scheduler{checker: checker, cfg: cfg, work: background.New(0, 0), enabled: loc != nil}
	if loc == nil {
		return s, nil
	}
	g, err := gocron.NewScheduler(
		gocron.WithLocation(loc),
		gocron.WithStopTimeout(background.SchedulerStopTimeout), // [SEC29-05][F293-02] 關機預算算進它
	)
	if err != nil {
		return nil, err
	}
	s.scheduler = g
	return s, nil
}

// alertJob 一個排程的定義.
type alertJob struct {
	name    string
	cron    string
	fetches int
	timeout time.Duration // Start 時算出
	check   func(context.Context) error
}

// Start 啟動排程：三個 cron 各自判斷（字面 off 不註冊），每個註冊的排程啟動時各跑一次.
func (s *Scheduler) Start() error {
	if !s.enabled {
		return nil
	}
	settings := s.cfg.CWASettings()
	jobs := []alertJob{
		{name: jobWarning, cron: settings.WarningCron, fetches: warningFetches, check: s.checker.CheckWarnings},
		{name: jobEarthquake, cron: settings.EarthquakeCron, fetches: earthquakeFetches, check: s.checker.CheckEarthquakes},
		{name: jobTsunami, cron: settings.TsunamiCron, fetches: tsunamiFetches, check: s.checker.CheckTsunami},
	}
	var started []alertJob
	for _, j := range jobs {
		if j.cron == config.CronOff {
			slog.Info("alert job disabled", "job", j.name)
			continue
		}
		j.timeout = s.jobTimeout(j)
		if _, err := s.scheduler.NewJob(
			gocron.CronJob(j.cron, false),
			gocron.NewTask(func() { s.work.Do(func(ctx context.Context) { s.runJob(ctx, j.name, j.timeout, j.check) }) }),
			gocron.WithName(j.name),
		); err != nil {
			return err
		}
		started = append(started, j)
	}

	s.scheduler.Start()
	slog.Info("alert scheduler started", "jobs", len(started))

	// 啟動後各排程非同步執行一次
	for _, j := range started {
		s.work.Go(func(ctx context.Context) {
			slog.Info("alert initial check started", "job", j.name)
			s.runJob(ctx, j.name, j.timeout, j.check)
		})
	}
	return nil
}

// jobTimeout 每個排程的 ctx 期限 = max(check_timeout_seconds（≤0 取 120 秒）, n × FetchBudget + 30 秒).
// 推導值勝出時印一次 Warn（只在 Start 呼叫，之後每輪不印）.
func (s *Scheduler) jobTimeout(j alertJob) time.Duration {
	configured := time.Duration(s.cfg.Alert.CheckTimeoutSeconds) * time.Second
	if configured <= 0 {
		configured = defaultCheckTimeout
	}
	derived := time.Duration(j.fetches)*s.checker.client().FetchBudget() + jobSlack
	if derived > configured {
		slog.Warn("alert job timeout raised to cover fetch budgets",
			"job", j.name, "configured_seconds", int(configured.Seconds()), "effective_seconds", int(derived.Seconds()))
		return derived
	}
	return configured
}

// Stop 停止排程，等進行中的檢查推播完（推到一半中斷的人重啟後會補送）；ctx 到期時取消.
func (s *Scheduler) Stop(ctx context.Context) error {
	if !s.enabled {
		return s.work.Shutdown(ctx)
	}
	return errors.Join(s.work.Shutdown(ctx), s.scheduler.Shutdown())
}

// runJob 跑一個排程的一輪檢查：期限、失敗 log 都在這裡（三個排程共用）.
func (s *Scheduler) runJob(ctx context.Context, job string, timeout time.Duration, check func(context.Context) error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := check(ctx); err != nil {
		// [F30-44] 氣象署錯誤只記分類，其他錯誤記遮蔽後的訊息.
		slog.Error("alert check failed", append([]any{"job", job}, failAttrs(err)...)...)
		return
	}

	// [F24-08] 正常完成不印 Info（正式環境每 5 分鐘一行雜訊）；失敗上面已印 Error
	slog.Debug("alert check completed", "job", job)
}
