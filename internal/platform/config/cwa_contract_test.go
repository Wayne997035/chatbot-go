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

// typeIs pins a value type at compile time: when the field type changes this call stops compiling.
func typeIs[T any](_ T) {}

func TestCWAContractFields(t *testing.T) {
	cc := config.CWAConfig{
		MaxAttempts: 1, DailyLimit: 1, SafetyRatio: 1, WarnRatio: 1, ExhaustedStatusCodes: "429",
	}
	typeIs[int](cc.MaxAttempts)
	typeIs[int](cc.DailyLimit)
	typeIs[float64](cc.SafetyRatio)
	typeIs[float64](cc.WarnRatio)
	typeIs[string](cc.ExhaustedStatusCodes)
	ac := config.AlertConfig{
		WarningCron: "a", EarthquakeCron: "b", TsunamiCron: "c", ColdIntervalMinutes: 1, EarthquakeLimit: 1,
	}
	typeIs[string](ac.WarningCron)
	typeIs[string](ac.EarthquakeCron)
	typeIs[string](ac.TsunamiCron)
	typeIs[int](ac.ColdIntervalMinutes)
	typeIs[int](ac.EarthquakeLimit)
	cfg := config.Config{CWA: cc, Alert: ac}
	typeIs[config.CWAConfig](cfg.CWA)

	s := config.CWASettings{
		WarningCron: "a", EarthquakeCron: "b", TsunamiCron: "c", ForecastCron: "d",
		ColdInterval: time.Minute, EarthquakeLimit: 1, MaxAttempts: 1, DailyLimit: 1,
		SafetyRatio: 1, WarnRatio: 1, ExhaustedStatus: []int{429},
	}
	typeIs[string](s.WarningCron)
	typeIs[string](s.EarthquakeCron)
	typeIs[string](s.TsunamiCron)
	typeIs[string](s.ForecastCron)
	typeIs[time.Duration](s.ColdInterval)
	typeIs[int](s.EarthquakeLimit)
	typeIs[int](s.MaxAttempts)
	typeIs[int](s.DailyLimit)
	typeIs[float64](s.SafetyRatio)
	typeIs[float64](s.WarnRatio)
	typeIs[[]int](s.ExhaustedStatus)
	if got := (&config.Config{}).CWASettings(); got.MaxAttempts != 2 {
		t.Fatalf("zero Config must give code defaults, got %+v", got)
	}
}
