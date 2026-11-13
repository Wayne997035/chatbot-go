package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
)

// startedScheduler 建立並啟動排程器（訂閱者為 subs），測試結束時停止. 啟動檢查在背景跑.
func startedScheduler(t *testing.T, cfg *config.Config, loc *time.Location, subs *fakeSubs) *Scheduler {
	t.Helper()
	s, err := NewScheduler(NewChecker(subs, newNotifier(), nil, cfg), cfg, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return s
}

func taipeiLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := cwa.LoadTaipei(nil)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// jobsByName 已註冊的 job（名稱 → job）.
func jobsByName(s *Scheduler) map[string]gocron.Job {
	out := map[string]gocron.Job{}
	for _, j := range s.scheduler.Jobs() {
		out[j.Name()] = j
	}
	return out
}

// gap 這個 job 相鄰兩次觸發的間隔.
func gap(t *testing.T, j gocron.Job) time.Duration {
	t.Helper()
	runs, err := j.NextRuns(3)
	if err != nil || len(runs) < 3 {
		t.Fatalf("next runs: %v %v", runs, err)
	}
	return runs[2].Sub(runs[1])
}

func feedConfig(f *routeFeed, mod func(*config.AlertConfig)) *config.Config {
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: f.srv.URL, CWAAuthKey: "test-key"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	if mod != nil {
		mod(&cfg.Alert)
	}
	return cfg
}

var wantDefaultGaps = map[string]time.Duration{
	jobWarning: 5 * time.Minute, jobEarthquake: 5 * time.Minute, jobTsunami: 30 * time.Minute,
}

// [F30-41] 明確寫出預設值：特報、地震每 5 分鐘，海嘯每 30 分鐘.
func TestAlertSchedulesDefaults(t *testing.T) {
	feed := newRouteFeed(t)
	cfg := feedConfig(feed, func(a *config.AlertConfig) {
		a.WarningCron, a.EarthquakeCron, a.TsunamiCron = "*/5 * * * *", "*/5 * * * *", "*/30 * * * *"
	})
	jobs := jobsByName(startedScheduler(t, cfg, taipeiLoc(t), &fakeSubs{}))
	for name, want := range wantDefaultGaps {
		j, ok := jobs[name]
		if !ok {
			t.Fatalf("job %s not registered", name)
		}
		if got := gap(t, j); got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
}

// [F30-41] 字面 off 只停用那一個排程，其餘照常.
func TestAlertScheduleOff(t *testing.T) {
	for _, off := range []string{jobWarning, jobEarthquake, jobTsunami} {
		t.Run(off, func(t *testing.T) {
			feed := newRouteFeed(t)
			cfg := feedConfig(feed, func(a *config.AlertConfig) {
				switch off {
				case jobWarning:
					a.WarningCron = config.CronOff
				case jobEarthquake:
					a.EarthquakeCron = config.CronOff
				case jobTsunami:
					a.TsunamiCron = config.CronOff
				}
			})
			jobs := jobsByName(startedScheduler(t, cfg, taipeiLoc(t), &fakeSubs{}))
			if len(jobs) != 2 {
				t.Errorf("jobs = %d, want 2", len(jobs))
			}
			if _, ok := jobs[off]; ok {
				t.Errorf("%s must not be registered when set to off", off)
			}
		})
	}
}

// [F30-41] 只認字面 off：OFF、帶空白的 off 視為無效值，退回預設（海嘯仍以 30 分鐘註冊）.
func TestAlertScheduleOffIsLiteralOnly(t *testing.T) {
	for _, v := range []string{"OFF", " off ", "Off"} {
		t.Run(v, func(t *testing.T) {
			feed := newRouteFeed(t)
			cfg := feedConfig(feed, func(a *config.AlertConfig) { a.TsunamiCron = v })
			j, ok := jobsByName(startedScheduler(t, cfg, taipeiLoc(t), &fakeSubs{}))[jobTsunami]
			if !ok {
				t.Fatalf("tsunami job must stay registered for %q (invalid value falls back to the default)", v)
			}
			if got := gap(t, j); got != 30*time.Minute {
				t.Errorf("interval = %v, want the default 30m", got)
			}
		})
	}
}

// [F30-41] cron 依傳入的時區解讀：每天 09:00 的下一次觸發在該時區是 09:00.
func TestAlertSchedulesUseTaipeiLocation(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for name, loc := range map[string]*time.Location{"Asia/Taipei": taipeiLoc(t), "America/New_York": newYork} {
		t.Run(name, func(t *testing.T) {
			feed := newRouteFeed(t)
			cfg := feedConfig(feed, func(a *config.AlertConfig) { a.EarthquakeCron = "0 9 * * *" })
			j, ok := jobsByName(startedScheduler(t, cfg, loc, &fakeSubs{}))[jobEarthquake]
			if !ok {
				t.Fatal("job not registered")
			}
			next, err := j.NextRun()
			if err != nil {
				t.Fatal(err)
			}
			if got := next.In(loc); got.Hour() != 9 || got.Minute() != 0 {
				t.Errorf("next run %v in %s, want 09:00", got, name)
			}
		})
	}
}

// [F30-41] 期限 = max(check_timeout_seconds（≤0 取 120 秒）, n × 單次抓取預算 + 30 秒).
func TestJobTimeoutDerivedFromAttempts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		checkSecs   int
		maxAttempts int
		job         alertJob
		want        time.Duration
	}{
		// 預設 2 次：單次預算 2×10+1 = 21 秒.
		{"unset falls back to 120s", 0, 0, alertJob{fetches: warningFetches}, 120 * time.Second},
		{"unset earthquake", 0, 0, alertJob{fetches: earthquakeFetches}, 120 * time.Second},
		{"unset tsunami", 0, 0, alertJob{fetches: tsunamiFetches}, 120 * time.Second},
		{"configured wins over derived 114s", 200, 0, alertJob{fetches: warningFetches}, 200 * time.Second},
		{"derived wins: 4×21+30", 60, 0, alertJob{fetches: warningFetches}, 114 * time.Second},
		{"derived wins: 2×21+30", 30, 0, alertJob{fetches: earthquakeFetches}, 72 * time.Second},
		{"derived wins: 1×21+30", 20, 0, alertJob{fetches: tsunamiFetches}, 51 * time.Second},
		// max_attempts=5：單次預算 5×10+4 = 54 秒，特報 4×54+30 = 246 > 120.
		{"max_attempts=5", 0, 5, alertJob{fetches: warningFetches}, 246 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feed := newRouteFeed(t)
			cfg := feedConfig(feed, func(a *config.AlertConfig) { a.CheckTimeoutSeconds = tc.checkSecs })
			cfg.CWA.MaxAttempts = tc.maxAttempts
			s, err := NewScheduler(NewChecker(&fakeSubs{}, newNotifier(), nil, cfg), cfg, taipeiLoc(t))
			if err != nil {
				t.Fatal(err)
			}
			if got := s.jobTimeout(tc.job); got != tc.want {
				t.Errorf("timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// [F30-41] 推導值勝出時只在 Start 印一次 Warn（每個 job 一行），之後每輪不印.
func TestTimeoutAdjustmentWarnsOnce(t *testing.T) {
	const msg = "alert job timeout raised"
	logs := captureText(t)
	feed := newRouteFeed(t)
	cfg := feedConfig(feed, func(a *config.AlertConfig) { a.CheckTimeoutSeconds = 10 })
	s := startedScheduler(t, cfg, taipeiLoc(t), &fakeSubs{})
	if got := strings.Count(logs.String(), msg); got != 3 {
		t.Fatalf("Start warned %d times, want 3 (one per job): %s", got, logs.String())
	}
	for _, job := range []string{jobWarning, jobEarthquake, jobTsunami} {
		if !strings.Contains(logs.String(), "job="+job) {
			t.Errorf("no warning for %s", job)
		}
	}
	for range 3 {
		s.runJob(context.Background(), jobTsunami, time.Minute, s.checker.CheckTsunami)
	}
	if got := strings.Count(logs.String(), msg); got != 3 {
		t.Errorf("after 3 more rounds warned %d times, want still 3", got)
	}

	quiet := captureText(t)
	cfg2 := feedConfig(feed, func(a *config.AlertConfig) { a.CheckTimeoutSeconds = 600 })
	startedScheduler(t, cfg2, taipeiLoc(t), &fakeSubs{})
	if strings.Contains(quiet.String(), msg) {
		t.Errorf("configured timeout covers the budgets, no warning expected: %s", quiet.String())
	}
}

// [F30-41] job 名稱固定：cwa-warning、cwa-earthquake、cwa-tsunami（舊的單一 job 名稱不再出現）.
func TestSchedulerJobNames(t *testing.T) {
	feed := newRouteFeed(t)
	jobs := jobsByName(startedScheduler(t, feedConfig(feed, nil), taipeiLoc(t), &fakeSubs{}))
	var got []string
	for name := range jobs {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := []string{"cwa-earthquake", "cwa-tsunami", "cwa-warning"}; !slices.Equal(got, want) {
		t.Errorf("job names = %v, want %v", got, want)
	}

	// CheckAndNotify 依序呼叫三個檢查並合併錯誤：三者都失敗時，錯誤含三者各自的失敗與分類.
	hang := newRouteFeed(t)
	for _, ds := range []string{"W-C0033-001", "E-A0015-001", "E-A0014-001"} {
		hang.hang[ds] = true
	}
	c := newFeedChecker(t, hang, weatherAndQuakeSubs(), newNotifier(), nil)
	c.WithCWAClient(optClient(cwa.ClientOptions{BaseURL: hang.srv.URL, AttemptTimeout: 40 * time.Millisecond}))
	err := c.CheckAndNotify(context.Background())
	if err == nil {
		t.Fatal("want a joined error")
	}
	for _, want := range []string{"weather warnings", "earthquakes", "tsunami warnings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error lacks %q: %v", want, err)
		}
	}
	if cwa.ClassOf(err) != "timeout" {
		t.Errorf("class = %q, want timeout (errors.As must see through the join)", cwa.ClassOf(err))
	}
}

// [F30-41] 設定全是零值（舊設定檔）= 預設頻率與 120 秒期限.
func TestZeroValuesMeanDefaults(t *testing.T) {
	cfg := &config.Config{}
	s := startedScheduler(t, cfg, taipeiLoc(t), &fakeSubs{})
	jobs := jobsByName(s)
	for name, want := range wantDefaultGaps {
		j, ok := jobs[name]
		if !ok {
			t.Fatalf("job %s not registered with zero config", name)
		}
		if got := gap(t, j); got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
	if got := s.jobTimeout(alertJob{fetches: tsunamiFetches}); got != defaultCheckTimeout {
		t.Errorf("timeout = %v, want %v", got, defaultCheckTimeout)
	}
}

// [F30-41] 時區載入失敗（loc 為 nil）：不註冊任何 job、不跑啟動檢查、不印 log、不打氣象署；Stop 不出錯.
func TestSchedulersDisabledWhenTaipeiUnavailable(t *testing.T) {
	logs := captureText(t)
	feed := newRouteFeed(t)
	s, err := NewScheduler(NewChecker(weatherAndQuakeSubs(), newNotifier(), nil, feedConfig(feed, nil)), feedConfig(feed, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start with nil location must not fail: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.scheduler != nil {
		t.Error("no gocron scheduler should exist without a location")
	}
	for _, ds := range []string{"W-C0033-001", "W-C0033-004", "E-A0015-001", "E-A0014-001"} {
		if feed.count(ds) != 0 {
			t.Errorf("%s requested %d times, want 0", ds, feed.count(ds))
		}
	}
	if out := logs.String(); out != "" {
		t.Errorf("package must not log when disabled (app logs once): %s", out)
	}
}
