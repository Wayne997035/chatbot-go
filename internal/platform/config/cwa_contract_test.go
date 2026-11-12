package config_test

import (
	"chatbot-go/internal/platform/config"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// F30-30 契約：D2 的 config 匯出名稱與型別；改一個字就編譯失敗.
var (
	_ func(*config.Config) config.CWASettings = (*config.Config).CWASettings
	_ func(string) (cron.Schedule, error)     = config.ParseCWACron
	_ string                                  = config.CronOff
)

func TestCWAContractFields(t *testing.T) {
	cc := config.CWAConfig{
		MaxAttempts: 1, DailyLimit: 1, SafetyRatio: 1, WarnRatio: 1, ExhaustedStatusCodes: "429",
	}
	var (
		_ int     = cc.MaxAttempts
		_ int     = cc.DailyLimit
		_ float64 = cc.SafetyRatio
		_ float64 = cc.WarnRatio
		_ string  = cc.ExhaustedStatusCodes
	)
	ac := config.AlertConfig{
		WarningCron: "a", EarthquakeCron: "b", TsunamiCron: "c", ColdIntervalMinutes: 1, EarthquakeLimit: 1,
	}
	var (
		_ string = ac.WarningCron
		_ string = ac.EarthquakeCron
		_ string = ac.TsunamiCron
		_ int    = ac.ColdIntervalMinutes
		_ int    = ac.EarthquakeLimit
	)
	cfg := config.Config{CWA: cc, Alert: ac}
	var _ config.CWAConfig = cfg.CWA

	s := config.CWASettings{
		WarningCron: "a", EarthquakeCron: "b", TsunamiCron: "c", ForecastCron: "d",
		ColdInterval: time.Minute, EarthquakeLimit: 1, MaxAttempts: 1, DailyLimit: 1,
		SafetyRatio: 1, WarnRatio: 1, ExhaustedStatus: []int{429},
	}
	var (
		_ string        = s.WarningCron
		_ string        = s.EarthquakeCron
		_ string        = s.TsunamiCron
		_ string        = s.ForecastCron
		_ time.Duration = s.ColdInterval
		_ int           = s.EarthquakeLimit
		_ int           = s.MaxAttempts
		_ int           = s.DailyLimit
		_ float64       = s.SafetyRatio
		_ float64       = s.WarnRatio
		_ []int         = s.ExhaustedStatus
	)
	if got := (&config.Config{}).CWASettings(); got.MaxAttempts != 2 {
		t.Fatalf("zero Config must give code defaults, got %+v", got)
	}
}
