package weather

import (
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// jobName 預報排程的 gocron job 名稱 [F30-51].
const jobName = "cwa-forecast"

// catchUpLookback 找「最近一次觸發時刻」往前看多久；cron 在這段時間內一次都沒觸發就視為判斷不了（fail-safe 補抓）.
const catchUpLookback = 48 * time.Hour

// latestCreateTimer 讀取元件的窄介面：各資料集最新的 createTime. 不加進 WeatherRepository，
// 以免測試用的假物件全部要改；沒實作 = 不知道 = 補抓 [F30-52].
type latestCreateTimer interface {
	LatestCreateTimes(ctx context.Context) (map[string]string, error)
}

// newForecastClient 沒有注入客戶端時用的預設客戶端（只有測試與未接線的情況會用到）.
func newForecastClient(cfg *config.Config) *cwa.Client {
	settings := cfg.CWASettings()
	return cwa.NewClient(cwa.ClientOptions{
		BaseURL:        strings.TrimRight(cfg.Weather.CWABaseURL, "/"),
		AuthKey:        cfg.Weather.CWAAuthKey,
		AttemptTimeout: cwa.ForecastAttemptTimeout,
	}, cwa.NewLedger(nil, settings, nil, time.Now), settings)
}

// Scheduler 天氣資料排程同步器.
type Scheduler struct {
	scheduler   gocron.Scheduler
	weatherRepo weatherstore.WeatherRepository
	cfg         *config.Config
	loc         *time.Location    // nil = 台灣時區載入失敗，不註冊任何排程.
	work        *background.Group // 啟動時的同步與排程中的同步
	now         func() time.Time

	clientOnce sync.Once
	client     *cwa.Client
}

// NewScheduler 建立排程器. loc 為 Asia/Taipei；為 nil（時區載入失敗）時 Start 不註冊 job、不補抓 [F30-51].
// client 為 nil 時第一次用到才建預設客戶端.
func NewScheduler(weatherRepo weatherstore.WeatherRepository, cfg *config.Config, client *cwa.Client, loc *time.Location) (*Scheduler, error) {
	opts := []gocron.SchedulerOption{gocron.WithStopTimeout(background.SchedulerStopTimeout)} // [SEC29-05][F293-02] 關機預算算進它
	if loc != nil {
		opts = append(opts, gocron.WithLocation(loc))
	}
	s, err := gocron.NewScheduler(opts...)
	if err != nil {
		return nil, err
	}

	return &Scheduler{
		scheduler:   s,
		weatherRepo: weatherRepo,
		cfg:         cfg,
		loc:         loc,
		work:        background.New(0, 0),
		now:         time.Now,
		client:      client,
	}, nil
}

func (s *Scheduler) cwaClient() *cwa.Client {
	s.clientOnce.Do(func() {
		if s.client == nil {
			s.client = newForecastClient(s.cfg)
		}
	})
	return s.client
}

// Start 啟動排程：定時 + 啟動時只在資料落後才補抓一次 [F30-51][F30-52].
func (s *Scheduler) Start() error {
	if s.loc == nil {
		slog.Warn("weather scheduler disabled", "reason", "timezone_unavailable")
		return nil
	}
	cron := s.cfg.CWASettings().ForecastCron
	if cron == config.CronOff {
		slog.Info("weather scheduler disabled", "reason", "off")
		return nil
	}

	_, err := s.scheduler.NewJob(
		gocron.CronJob(cron, false),
		gocron.NewTask(func() { s.work.Do(s.syncWeather) }),
		gocron.WithName(jobName),
	)
	if err != nil {
		return err
	}

	s.scheduler.Start()
	slog.Info("weather scheduler started", "cron", cron)

	// 啟動後非同步檢查資料是否落後；落後才補抓一輪（Render 喚醒都會重啟，無條件補抓每次吃額度）
	s.work.Go(func(ctx context.Context) {
		if !s.needsCatchUp(ctx, cron) {
			slog.Info("weather initial sync skipped", "reason", "data_fresh")
			return
		}
		slog.Info("weather initial sync started")
		s.syncWeather(ctx)
	})

	return nil
}

// lastTrigger 回 cron 在 now 以前（含）最近一次的觸發時刻；找不到回零值.
func lastTrigger(cron string, now time.Time) time.Time {
	sched, err := config.ParseCWACron(cron)
	if err != nil {
		return time.Time{}
	}
	var last time.Time
	for t := sched.Next(now.Add(-catchUpLookback)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		last = t
	}
	return last
}

// needsCatchUp 23 份資料集各自最新 createTime 的最小值早於最近觸發時刻，才需要補抓.
// 任何一種判斷不了（讀取元件沒實作、查詢出錯、缺任一份、解析失敗）一律補抓：多抓不漏抓.
func (s *Scheduler) needsCatchUp(ctx context.Context, cron string) bool {
	trigger := lastTrigger(cron, s.now())
	if trigger.IsZero() {
		return true
	}
	repo, ok := s.weatherRepo.(latestCreateTimer)
	if !ok {
		return true
	}
	latest, err := repo.LatestCreateTimes(ctx)
	if err != nil {
		slog.Warn("weather latest createTime query failed, syncing", "error", redact.Error(err))
		return true
	}
	for _, id := range forecastDatasets() {
		raw, found := latest[id]
		if !found {
			return true
		}
		t, err := time.ParseInLocation(createTimeLayout, raw, s.loc)
		if err != nil || t.Before(trigger) {
			return true
		}
	}
	return false
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

	// 完成 / 略過的 log 由 FetchAndStore 印（帶三個計數）.
	if _, err := FetchAndStore(ctx, s.cwaClient(), s.cfg.Weather.RateLimitMs, s.weatherRepo); err != nil {
		slog.Error("weather sync failed", errAttr(err))
	}
}
