package weather

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/labstack/echo/v4"
	"gopkg.in/yaml.v3"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

const fcstProductionCron = "40 5,11,17,23 * * *"

func fcstTaipei(t *testing.T) *time.Location {
	t.Helper()
	loc, err := cwa.LoadTaipei(nil)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// fcstUTCLocal 把 time.Local 設成 UTC（容器沒設 TZ 的情況），測完還原.
func fcstUTCLocal(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
}

// fcstStartScheduler 建立並啟動預報排程；測完停止. repo 為 nil 用空的 fcstPlainRepo.
func fcstStartScheduler(t *testing.T, repo weatherstore.WeatherRepository, cfg *config.Config, srvURL string, loc *time.Location) *Scheduler {
	t.Helper()
	cfg.Weather.CWABaseURL = srvURL
	s, err := NewScheduler(repo, cfg, fcstClient(srvURL, nil, 0), loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// fcstTaipeiPattern 每個時刻換成 Asia/Taipei 後分 40、秒 0、時屬於 {5,11,17,23}，且相鄰恰 6 小時.
func fcstTaipeiPattern(runs []time.Time, loc *time.Location) bool {
	for i, r := range runs {
		l := r.In(loc)
		if l.Minute() != 40 || l.Second() != 0 || !slices.Contains([]int{5, 11, 17, 23}, l.Hour()) {
			return false
		}
		if i > 0 && r.Sub(runs[i-1]) != 6*time.Hour {
			return false
		}
	}
	return len(runs) > 0
}

// F30-51：排程在台灣時間 05:40 / 11:40 / 17:40 / 23:40 觸發；容器本地時區是 UTC 也一樣.
func TestWeatherScheduleRunsAtTaipeiTimes(t *testing.T) {
	fcstUTCLocal(t)
	taipei := fcstTaipei(t)
	srv := fcstNewServer(t, nil)

	t.Run("with Taipei location", func(t *testing.T) {
		s := fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{Weather: config.WeatherConfig{Cron: fcstProductionCron}}, srv.URL, taipei)
		jobs := s.scheduler.Jobs()
		if len(jobs) != 1 {
			t.Fatalf("jobs = %d", len(jobs))
		}
		runs, err := jobs[0].NextRuns(8)
		if err != nil {
			t.Fatal(err)
		}
		if !fcstTaipeiPattern(runs, taipei) {
			t.Fatalf("runs (in Taipei) do not follow 05:40/11:40/17:40/23:40: %v", runs)
		}
	})

	// 正對照：沒設 WithLocation 的排程在 UTC 下會是台灣時間 13:40 等，不符合上面的性質；
	// 若這裡也符合，代表這個測試沒在區分時區.
	t.Run("control without location must not match", func(t *testing.T) {
		ctl, err := gocron.NewScheduler()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ctl.Shutdown() }()
		job, err := ctl.NewJob(gocron.CronJob(fcstProductionCron, false), gocron.NewTask(func() {}))
		if err != nil {
			t.Fatal(err)
		}
		runs, err := job.NextRuns(8)
		if err != nil {
			t.Fatal(err)
		}
		if fcstTaipeiPattern(runs, taipei) {
			t.Fatalf("control schedule without WithLocation matched the Taipei pattern, so this test cannot tell timezones apart: %v", runs)
		}
	})
}

// F30-51：production.yaml 的預報 cron 是台灣時間發布後 10 分鐘，且建出來的排程照它跑.
func TestProductionWeatherCron(t *testing.T) {
	data, err := os.ReadFile("../../configs/production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Weather.Cron != fcstProductionCron {
		t.Fatalf("weather.cron = %q, want %q", cfg.Weather.Cron, fcstProductionCron)
	}
	fcstUTCLocal(t)
	taipei := fcstTaipei(t)
	srv := fcstNewServer(t, nil)
	cfg.Weather.RateLimitMs = 0
	s := fcstStartScheduler(t, &fcstPlainRepo{}, cfg, srv.URL, taipei)
	runs, err := s.scheduler.Jobs()[0].NextRuns(8)
	if err != nil {
		t.Fatal(err)
	}
	if !fcstTaipeiPattern(runs, taipei) {
		t.Fatalf("scheduler built from production.yaml does not follow the Taipei pattern: %v", runs)
	}
}

// F30-51：job 名稱是 cwa-forecast（取代 weather-sync）.
func TestSchedulerJobNames(t *testing.T) {
	srv := fcstNewServer(t, nil)
	s := fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{}, srv.URL, fcstTaipei(t))
	var names []string
	for _, j := range s.scheduler.Jobs() {
		names = append(names, j.Name())
	}
	if !slices.Equal(names, []string{"cwa-forecast"}) {
		t.Fatalf("job names = %v", names)
	}
}

// F30-51：時區載入失敗（loc 為 nil）→ 不註冊 job、不補抓、Start 回 nil；對照：有時區且 DB 為空會補抓.
func TestSchedulersDisabledWhenTaipeiUnavailable(t *testing.T) {
	t.Run("nil location disables everything", func(t *testing.T) {
		srv := fcstNewServer(t, nil)
		s := fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{}, srv.URL, nil)
		time.Sleep(300 * time.Millisecond)
		if n := len(s.scheduler.Jobs()); n != 0 || srv.Total() != 0 {
			t.Fatalf("jobs = %d, requests = %d, want 0 and 0", n, srv.Total())
		}
	})
	t.Run("control with Taipei and empty DB syncs", func(t *testing.T) {
		srv := fcstNewServer(t, nil)
		fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{}, srv.URL, fcstTaipei(t))
		fcstWaitFor(t, "a request", func() bool { return srv.Total() >= 1 })
	})
}

// F30-51：字面量 Config（零值）= 預設 cron，註冊 1 個 job，且不印 invalid CWA setting.
func TestZeroValuesMeanDefaults(t *testing.T) {
	logs := fcstCaptureLogs(t)
	srv := fcstNewServer(t, nil)
	s := fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{}, srv.URL, fcstTaipei(t))
	jobs := s.scheduler.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("zero-value config must still register the forecast job, got %d jobs", len(jobs))
	}
	runs, err := jobs[0].NextRuns(4)
	if err != nil {
		t.Fatal(err)
	}
	if !fcstTaipeiPattern(runs, fcstTaipei(t)) {
		t.Errorf("default cron runs = %v", runs)
	}
	if strings.Contains(logs.String(), "invalid CWA setting") {
		t.Errorf("zero values must not warn: %s", logs.String())
	}
}

// F30-51：cron 寫字面 off → 不註冊、不補抓；對照：預設 cron 且 DB 為空會補抓.
func TestWeatherScheduleOff(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		srv := fcstNewServer(t, nil)
		s := fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{Weather: config.WeatherConfig{Cron: config.CronOff}}, srv.URL, fcstTaipei(t))
		time.Sleep(300 * time.Millisecond)
		if n := len(s.scheduler.Jobs()); n != 0 || srv.Total() != 0 {
			t.Fatalf("jobs = %d, requests = %d, want 0 and 0", n, srv.Total())
		}
	})
	t.Run("control default cron syncs", func(t *testing.T) {
		srv := fcstNewServer(t, nil)
		fcstStartScheduler(t, &fcstPlainRepo{}, &config.Config{}, srv.URL, fcstTaipei(t))
		fcstWaitFor(t, "a request", func() bool { return srv.Total() >= 1 })
	})
}

// fcstFixedNow 2026-11-10 05:41（台灣時間）：最近觸發時刻是 05:40.
func fcstFixedNow(t *testing.T, hour, minute int) func() time.Time {
	t.Helper()
	taipei := fcstTaipei(t)
	return func() time.Time { return time.Date(2026, 11, 10, hour, minute, 0, 0, taipei) }
}

// fcstLatest 23 份資料集的最新 createTime 都是 value.
func fcstLatest(value string) map[string]string {
	m := map[string]string{}
	for _, id := range fcstWeekDatasets() {
		m[id] = value
	}
	return m
}

func fcstStartAt(t *testing.T, repo weatherstore.WeatherRepository, srv *fcstServer, now func() time.Time) *Scheduler {
	t.Helper()
	cfg := &config.Config{Weather: config.WeatherConfig{CWABaseURL: srv.URL}}
	s, err := NewScheduler(repo, cfg, fcstClient(srv.URL, nil, 0), fcstTaipei(t))
	if err != nil {
		t.Fatal(err)
	}
	s.now = now
	t.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	return s
}

// F30-52：任何一份落後（最小值 05:35 < 05:40）就補抓完整一輪（23 次，不是 45）.
func TestStartupSyncRunsWhenAnyDatasetStale(t *testing.T) {
	logs := fcstCaptureLogs(t)
	latest := fcstLatest("2026-11-10 05:41:00")
	latest["F-D0047-051"] = "2026-11-10 05:35:00" // 半途中斷的同步：只有這一份是舊的
	srv := fcstNewServer(t, nil)
	fcstStartAt(t, &fcstLatestRepo{latest: latest}, srv, fcstFixedNow(t, 5, 41))

	fcstWaitFor(t, "sync completed", func() bool { return strings.Contains(logs.String(), "weather sync completed") })
	if srv.Total() != 23 {
		t.Fatalf("requests = %d, want 23", srv.Total())
	}
}

// F30-52：23 份都 ≥ 最近觸發時刻 → 不補抓，但排程 job 仍註冊.
func TestStartupSyncSkippedWhenAllFresh(t *testing.T) {
	logs := fcstCaptureLogs(t)
	srv := fcstNewServer(t, nil)
	s := fcstStartAt(t, &fcstLatestRepo{latest: fcstLatest("2026-11-10 05:41:00")}, srv, fcstFixedNow(t, 5, 50))

	fcstWaitFor(t, "initial sync decision", func() bool { return strings.Contains(logs.String(), "weather initial sync skipped") })
	if srv.Total() != 0 {
		t.Fatalf("requests = %d, want 0", srv.Total())
	}
	if len(s.scheduler.Jobs()) != 1 {
		t.Fatalf("jobs = %d, want the forecast job still registered", len(s.scheduler.Jobs()))
	}
}

// F30-52：邊界：最舊那份恰為 05:40:00 不補抓（>=）；05:39:59 補抓；只比日期的作法在後兩列會錯.
func TestStartupSyncBoundary(t *testing.T) {
	tests := []struct {
		name   string
		oldest string
		want   bool
	}{
		{"exactly at trigger", "2026-11-10 05:40:00", false},
		{"one second before trigger", "2026-11-10 05:39:59", true},
		{"previous day same clock", "2026-11-09 05:41:00", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest := fcstLatest("2026-11-10 05:41:00")
			latest["F-D0047-003"] = tt.oldest
			s, err := NewScheduler(&fcstLatestRepo{latest: latest}, &config.Config{}, nil, fcstTaipei(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop(context.Background()) })
			s.now = fcstFixedNow(t, 5, 41)
			if got := s.needsCatchUp(context.Background(), fcstProductionCron); got != tt.want {
				t.Errorf("needsCatchUp = %v, want %v", got, tt.want)
			}
		})
	}
}

// F30-52：fail-safe 五案例都補抓（各 23 次請求）；查詢出錯另有一行 Warn，不含位址.
func TestStartupSyncFailSafe(t *testing.T) {
	missing089 := fcstLatest("2026-11-10 05:41:00")
	delete(missing089, "F-D0047-089")
	tests := []struct {
		name string
		repo weatherstore.WeatherRepository
		warn bool
	}{
		{"no documents at all", &fcstLatestRepo{latest: map[string]string{}}, false},
		{"089 missing", &fcstLatestRepo{latest: missing089}, false},
		{"query error", &fcstLatestRepo{err: &fcstAddrError{"dial tcp 10.0.0.9:27017: connection refused"}}, true},
		{"unparsable time", &fcstLatestRepo{latest: fcstLatest("not a time")}, false},
		{"repo without the narrow interface", &fcstPlainRepo{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := fcstCaptureLogs(t)
			srv := fcstNewServer(t, nil)
			fcstStartAt(t, tt.repo, srv, fcstFixedNow(t, 5, 41))
			fcstWaitFor(t, "sync completed", func() bool { return strings.Contains(logs.String(), "weather sync completed") })
			if srv.Total() != 23 {
				t.Errorf("requests = %d, want 23", srv.Total())
			}
			out := logs.String()
			if tt.warn && !strings.Contains(out, "weather latest createTime query failed") {
				t.Errorf("want a Warn for the failed query: %s", out)
			}
			if strings.Contains(out, "10.0.0.9") {
				t.Errorf("log leaks address: %s", out)
			}
		})
	}
}

// fcstAddrError 帶位址字樣的假錯誤（驗證 log 會被遮蔽）.
type fcstAddrError struct{ msg string }

func (e *fcstAddrError) Error() string { return e.msg }

// F30-53：手動觸發不看資料新鮮度：資料全新鮮時仍抓完整一輪.
func TestManualTriggerIgnoresFreshness(t *testing.T) {
	srv := fcstNewServer(t, nil)
	repo := &fcstLatestRepo{latest: fcstLatest("2099-01-01 00:00:00")}
	h := NewWeatherHandler(repo, &config.Config{Weather: config.WeatherConfig{CWABaseURL: srv.URL}}).
		WithCWAClient(fcstClient(srv.URL, nil, 0))
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
	rec := httptest.NewRecorder()
	if err := h.TriggerSync(echo.New().NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	fcstWaitFor(t, "23 requests", func() bool { return srv.Total() >= 23 })
	if srv.Total() != 23 {
		t.Fatalf("requests = %d, want 23", srv.Total())
	}
}
