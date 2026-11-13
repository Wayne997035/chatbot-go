package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newMemLedger(t *testing.T, clk *fakeClock, s *config.CWASettings) *Ledger {
	t.Helper()
	return NewLedger(nil, s, taipeiLoc(t), clk.Now)
}

// bruteColdPlanned 以逐分鐘暴力推算預定呼叫數（獨立於 Ledger.planned 的實作），cron 以 matcher 表示.
func brutePlanned(now time.Time, loc *time.Location, warn, quake, tsunami func(time.Time) bool, forecast bool,
	cold time.Duration,
) int {
	total, nextCold := 0, now
	for m := 1; m <= 24*60; m++ {
		at := now.Add(time.Duration(m) * time.Minute)
		lt := at.In(loc)
		if quake != nil && quake(lt) {
			total++
		}
		if tsunami != nil && tsunami(lt) {
			total++
		}
		if forecast && lt.Minute() == 40 && (lt.Hour() == 5 || lt.Hour() == 11 || lt.Hour() == 17 || lt.Hour() == 23) {
			total += ForecastDatasetCount
		}
		if warn != nil && warn(lt) {
			total++
			if !at.Before(nextCold) {
				total++
				nextCold = at.Add(cold)
			}
		}
	}
	return total
}

func every(n int) func(time.Time) bool {
	return func(t time.Time) bool { return t.Minute()%n == 0 }
}

// F30-34.
func TestPlannedCallsFollowConfig(t *testing.T) {
	clk := newTestClock(t)
	loc := taipeiLoc(t)
	now := clk.Now()
	min45 := func(t time.Time) bool { return t.Minute() == 0 || t.Minute() == 45 }
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   int
	}{
		{"default", nil, 764},
		{"earthquake every minute", func(c *config.Config) { c.Alert.EarthquakeCron = "* * * * *" }, 1916},
		{"cold interval 60", func(c *config.Config) { c.Alert.ColdIntervalMinutes = 60 }, 740},
		{"forecast off", func(c *config.Config) { c.Weather.Cron = config.CronOff }, 672},
		{"warning off also zeroes cold", func(c *config.Config) { c.Alert.WarningCron = config.CronOff }, 428},
		{
			"warning every 45 simulated",
			func(c *config.Config) { c.Alert.WarningCron = "*/45 * * * *" },
			brutePlanned(now, loc, min45, every(5), every(30), true, 30*time.Minute),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newMemLedger(t, clk, testSettings(tc.mutate))
			if got := l.planned(now)[24]; got != tc.want {
				t.Fatalf("planned_24 = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("nil location plans nothing", func(t *testing.T) {
		l := NewLedger(nil, testSettings(nil), nil, clk.Now)
		if got := l.planned(now)[24]; got != 0 {
			t.Fatalf("planned_24 = %d, want 0 without a timezone", got)
		}
	})
	t.Run("monotonic in k", func(t *testing.T) {
		l := newMemLedger(t, clk, testSettings(nil))
		p := l.planned(now)
		for k := 1; k < len(p); k++ {
			if p[k] < p[k-1] {
				t.Fatalf("planned not cumulative at k=%d: %v", k, p)
			}
		}
	})
}

func defaultNearFull(l *Ledger, clk *fakeClock) {
	seed(l, hourOf(clk), map[string]int{StreamEarthquake: 900})
}

// F30-34.
func TestGateNearFullDeniesRetriesNotFirstCalls(t *testing.T) {
	ctx := context.Background()
	calls := []struct {
		name    string
		spec    CallSpec
		attempt int
	}{
		{"b earthquake retry", CallSpec{Stream: StreamEarthquake, Exempt: true, Dataset: "E-A0015-001"}, 2},
		{"c cold", CallSpec{Stream: StreamCold, Dataset: "W-C0033-004"}, 1},
		{"d forecast", CallSpec{Stream: StreamForecast, Dataset: "F-D0047-003"}, 1},
		{"e township detail", CallSpec{Stream: StreamWarning, Dataset: "W-C0033-003"}, 1},
	}
	t.Run("near full", func(t *testing.T) {
		logs := captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		defaultNearFull(l, clk)
		if err := l.reserve(ctx, CallSpec{Stream: StreamEarthquake, Exempt: true}, 1); err != nil {
			t.Fatalf("(a) exempt first call must go out: %v", err)
		}
		for _, c := range calls {
			if err := l.reserve(ctx, c.spec, c.attempt); !errors.Is(err, ErrQuotaReserved) {
				t.Fatalf("%s: err = %v, want ErrQuotaReserved", c.name, err)
			}
		}
		out := logs.String()
		if !strings.Contains(out, "cwa fetch skipped") || !strings.Contains(out, "reason=quota_reserved") ||
			!strings.Contains(out, "cap=950") {
			t.Fatalf("skip log missing fields: %s", out)
		}
		for _, bad := range []string{"127.0.0.1", "http://", "?format=", "opendata.cwa.gov.tw", "SECRET", "W-C0033"} {
			if strings.Contains(out, bad) {
				t.Fatalf("skip log leaks %q: %s", bad, out)
			}
		}
	})
	t.Run("control: 700 spread over 24h lets everything through", func(t *testing.T) {
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		for h := hourOf(clk) - 24; h <= hourOf(clk); h++ {
			seed(l, h, map[string]int{StreamForecast: 28})
		}
		for _, c := range calls {
			if err := l.reserve(ctx, c.spec, c.attempt); err != nil {
				t.Fatalf("%s denied with comfortable margin: %v", c.name, err)
			}
		}
	})
}

// F30-34.
func TestGateDecisionChangesWithFrequency(t *testing.T) {
	ctx := context.Background()
	retry := CallSpec{Stream: StreamEarthquake, Exempt: true}
	clk := newTestClock(t)
	def := newMemLedger(t, clk, testSettings(nil))
	if err := def.reserve(ctx, retry, 2); err != nil {
		t.Fatalf("default settings, used=0: retry must pass, got %v", err)
	}
	dense := newMemLedger(t, clk, testSettings(func(c *config.Config) { c.Alert.EarthquakeCron = "* * * * *" }))
	if err := dense.reserve(ctx, retry, 2); !errors.Is(err, ErrQuotaReserved) {
		t.Fatalf("every-minute earthquake (1916+1 > 950): retry must be denied, got %v", err)
	}
}

// F30-34.
func TestGatePriorityReservesForHigherTier(t *testing.T) {
	ctx := context.Background()
	// 預設設定：cap 950、planned_24 764 → used 146 時最小餘裕恰為 40.
	build := func(t *testing.T, quakeRetries int) *Ledger {
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		seed(l, hourOf(clk), map[string]int{StreamForecast: 146, StreamEarthquake + ":retry": quakeRetries})
		return l
	}
	calls := []struct {
		name string
		spec CallSpec
		deny bool // 第 1 級預留 50 時是否被擋.
	}{
		{"tier1 earthquake", CallSpec{Stream: StreamEarthquake}, false},
		{"tier1 tsunami", CallSpec{Stream: StreamTsunami}, false},
		{"tier2 warning", CallSpec{Stream: StreamWarning}, true},
		{"tier3 cold", CallSpec{Stream: StreamCold}, true},
		{"tier3 forecast", CallSpec{Stream: StreamForecast}, true},
	}
	for _, c := range calls {
		t.Run(c.name+" with 50 higher-tier retries", func(t *testing.T) {
			err := build(t, 50).reserve(ctx, c.spec, 2)
			if c.deny != errors.Is(err, ErrQuotaReserved) {
				t.Fatalf("deny=%v but err=%v", c.deny, err)
			}
		})
		t.Run(c.name+" control with 0 higher-tier retries", func(t *testing.T) {
			if err := build(t, 0).reserve(ctx, c.spec, 2); err != nil {
				t.Fatalf("no reserve needed, got %v", err)
			}
		})
	}
	t.Run("tier3 reserve adds tier2 retries", func(t *testing.T) {
		l := build(t, 0)
		seed(l, hourOf(&fakeClock{t: l.now()}), map[string]int{StreamWarning + ":retry": 45})
		if err := l.reserve(ctx, CallSpec{Stream: StreamCold}, 2); !errors.Is(err, ErrQuotaReserved) {
			t.Fatalf("45 tier-2 retries reserve 46 > 40 for tier 3, got %v", err)
		}
		if err := l.reserve(ctx, CallSpec{Stream: StreamWarning}, 2); err != nil {
			t.Fatalf("tier 2 is not reserved against its own retries: %v", err)
		}
	})
}

// F30-34：餘裕恰 10，50 個 goroutine 同時各發 1 次非免擋呼叫，恰 10 個通過.
func TestGateReserveIsAtomic(t *testing.T) {
	clk := newTestClock(t)
	s := testSettings(func(c *config.Config) {
		c.Alert.WarningCron, c.Alert.EarthquakeCron, c.Alert.TsunamiCron = config.CronOff, config.CronOff, config.CronOff
		c.Weather.Cron = config.CronOff
		c.CWA.DailyLimit, c.CWA.SafetyRatio, c.CWA.WarnRatio = 10, 1, 0.5
	})
	l := newMemLedger(t, clk, s) // cap 10、planned 0、used 0 → 餘裕恰 10.
	var passed, denied atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := l.reserve(context.Background(), CallSpec{Stream: StreamForecast}, 1)
			switch {
			case err == nil:
				passed.Add(1)
			case errors.Is(err, ErrQuotaReserved):
				denied.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if passed.Load() != 10 || denied.Load() != 40 {
		t.Fatalf("passed=%d denied=%d, want 10/40", passed.Load(), denied.Load())
	}
}

// F30-34.
func TestStartupWarnsWhenPlannedExceedsCap(t *testing.T) {
	clk := newTestClock(t)
	t.Run("dense config warns with both numbers", func(t *testing.T) {
		logs := captureLogs(t)
		s := testSettings(func(c *config.Config) { c.Alert.EarthquakeCron = "* * * * *" })
		l := newMemLedger(t, clk, s)
		if out := logs.String(); !strings.Contains(out, "planned=1916") || !strings.Contains(out, "cap=950") ||
			strings.Count(out, "level=WARN") != 1 {
			t.Fatalf("want one Warn with planned=1916 cap=950, got: %s", out)
		}
		if l.s.EarthquakeCron != "* * * * *" {
			t.Fatalf("settings must not be altered, got %q", l.s.EarthquakeCron)
		}
	})
	t.Run("default config is silent", func(t *testing.T) {
		logs := captureLogs(t)
		newMemLedger(t, clk, testSettings(nil))
		if logs.String() != "" {
			t.Fatalf("unexpected log: %s", logs.String())
		}
	})
}

// F30-34：同一 stream 一小時至多一行.
func TestGateSkippedLogOncePerHour(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	clk := newTestClock(t)
	l := newMemLedger(t, clk, testSettings(nil))
	defaultNearFull(l, clk)
	cold := CallSpec{Stream: StreamCold}
	for range 3 {
		_ = l.reserve(ctx, cold, 1)
	}
	_ = l.reserve(ctx, CallSpec{Stream: StreamForecast}, 1)
	count := func(stream string) int {
		return strings.Count(logs.String(), fmt.Sprintf(`msg="cwa fetch skipped" stream=%s`, stream))
	}
	if count("cold") != 1 || count("forecast") != 1 {
		t.Fatalf("want one line per stream, got cold=%d forecast=%d:\n%s", count("cold"), count("forecast"), logs)
	}
	clk.Advance(time.Hour)
	_ = l.reserve(ctx, cold, 1)
	if count("cold") != 2 {
		t.Fatalf("a new hour must log again, got %d", count("cold"))
	}
}
