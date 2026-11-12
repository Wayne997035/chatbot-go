package config

import (
	"bytes"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// captureLogs 把預設 slog 導到 buffer，測完還原.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// loadLike 模擬 Load 對氣象署設定做的事（環境變數覆蓋 + 驗證）.
func loadLike(c *Config) { applyCWASettings(c) }

func defaultSettings() CWASettings {
	return CWASettings{
		WarningCron: "*/5 * * * *", EarthquakeCron: "*/5 * * * *", TsunamiCron: "*/30 * * * *",
		ForecastCron: "40 5,11,17,23 * * *", ColdInterval: 30 * time.Minute, EarthquakeLimit: 5,
		MaxAttempts: 2, DailyLimit: 1000, SafetyRatio: 0.95, WarnRatio: 0.8, ExhaustedStatus: []int{429},
	}
}

// F30-36.
func TestCWASettingsEnvOverride(t *testing.T) {
	tests := []struct {
		env, val string
		check    func(CWASettings) bool
	}{
		{"CWA_WARNING_CRON", "*/10 * * * *", func(s CWASettings) bool { return s.WarningCron == "*/10 * * * *" }},
		{"CWA_EARTHQUAKE_CRON", "*/7 * * * *", func(s CWASettings) bool { return s.EarthquakeCron == "*/7 * * * *" }},
		{"CWA_TSUNAMI_CRON", "off", func(s CWASettings) bool { return s.TsunamiCron == CronOff }},
		{"CWA_COLD_INTERVAL_MINUTES", "60", func(s CWASettings) bool { return s.ColdInterval == time.Hour }},
		{"CWA_EARTHQUAKE_LIMIT", "3", func(s CWASettings) bool { return s.EarthquakeLimit == 3 }},
		{"CWA_FORECAST_CRON", "0 8 * * *", func(s CWASettings) bool { return s.ForecastCron == "0 8 * * *" }},
		{"CWA_MAX_ATTEMPTS", "4", func(s CWASettings) bool { return s.MaxAttempts == 4 }},
		{"CWA_DAILY_LIMIT", "500", func(s CWASettings) bool { return s.DailyLimit == 500 }},
		{"CWA_SAFETY_RATIO", "0.9", func(s CWASettings) bool { return s.SafetyRatio == 0.9 }},
		{"CWA_WARN_RATIO", "0.5", func(s CWASettings) bool { return s.WarnRatio == 0.5 }},
		{"CWA_EXHAUSTED_STATUS", "403, 429", func(s CWASettings) bool { return reflect.DeepEqual(s.ExhaustedStatus, []int{403, 429}) }},
	}
	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			logs := captureLogs(t)
			t.Setenv(tc.env, tc.val)
			c := &Config{}
			loadLike(c)
			if s := c.CWASettings(); !tc.check(s) {
				t.Fatalf("%s=%q ignored: %+v", tc.env, tc.val, s)
			}
			if strings.Contains(logs.String(), "invalid CWA setting") {
				t.Fatalf("unexpected warn: %s", logs)
			}
		})
	}
	t.Run("empty env keeps YAML value", func(t *testing.T) {
		t.Setenv("CWA_MAX_ATTEMPTS", "")
		t.Setenv("CWA_WARNING_CRON", "")
		c := &Config{CWA: CWAConfig{MaxAttempts: 3}, Alert: AlertConfig{WarningCron: "*/15 * * * *"}}
		loadLike(c)
		if s := c.CWASettings(); s.MaxAttempts != 3 || s.WarningCron != "*/15 * * * *" {
			t.Fatalf("empty env overrode YAML: %+v", s)
		}
	})
}

// F30-36.
func TestInvalidCWASettingsFallBackToDefaults(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		field  string
		reason string
	}{
		{"cron text", func(c *Config) { c.Alert.WarningCron = "not a cron" }, "alert.warning_cron", "bad_cron"},
		{"cron zeros", func(c *Config) { c.Alert.EarthquakeCron = "0 0 0 0 0" }, "alert.earthquake_cron", "bad_cron"},
		{"forecast cron", func(c *Config) { c.Weather.Cron = "@daily" }, "weather.cron", "bad_cron"},
		{"tsunami cron OFF", func(c *Config) { c.Alert.TsunamiCron = "OFF" }, "alert.tsunami_cron", "bad_cron"},
		{"safety high", func(c *Config) { c.CWA.SafetyRatio = 1.5 }, "cwa.safety_ratio", "out_of_range"},
		{"safety negative", func(c *Config) { c.CWA.SafetyRatio = -0.1 }, "cwa.safety_ratio", "out_of_range"},
		{"warn above safety", func(c *Config) { c.CWA.WarnRatio = 0.99 }, "cwa.warn_ratio", "warn_above_safety"},
		{"daily negative", func(c *Config) { c.CWA.DailyLimit = -5 }, "cwa.daily_limit", "out_of_range"},
		{"quake limit high", func(c *Config) { c.Alert.EarthquakeLimit = 51 }, "alert.earthquake_limit", "out_of_range"},
		{"quake limit negative", func(c *Config) { c.Alert.EarthquakeLimit = -1 }, "alert.earthquake_limit", "out_of_range"},
		{"attempts high", func(c *Config) { c.CWA.MaxAttempts = 6 }, "cwa.max_attempts", "out_of_range"},
		{"attempts negative", func(c *Config) { c.CWA.MaxAttempts = -1 }, "cwa.max_attempts", "out_of_range"},
		{"cold too small", func(c *Config) { c.Alert.ColdIntervalMinutes = 4 }, "alert.cold_interval_minutes", "out_of_range"},
		{"status 200", func(c *Config) { c.CWA.ExhaustedStatusCodes = "200" }, "cwa.exhausted_status_codes", "bad_status"},
		{"status text", func(c *Config) { c.CWA.ExhaustedStatusCodes = "abc" }, "cwa.exhausted_status_codes", "bad_status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			c := &Config{}
			tc.mutate(c)
			loadLike(c) // 不回錯、不 panic.
			if got := c.CWASettings(); !reflect.DeepEqual(got, defaultSettings()) {
				t.Fatalf("not default: %+v", got)
			}
			out := logs.String()
			if strings.Count(out, "invalid CWA setting") != 1 ||
				!strings.Contains(out, "field="+tc.field) || !strings.Contains(out, "reason="+tc.reason) {
				t.Fatalf("want one warn field=%s reason=%s, got: %s", tc.field, tc.reason, out)
			}
		})
	}
	t.Run("env not an integer", func(t *testing.T) {
		logs := captureLogs(t)
		t.Setenv("CWA_MAX_ATTEMPTS", "two")
		t.Setenv("CWA_SAFETY_RATIO", "abc")
		c := &Config{CWA: CWAConfig{MaxAttempts: 3}}
		loadLike(c)
		if got := c.CWASettings(); got.MaxAttempts != 2 || got.SafetyRatio != 0.95 {
			t.Fatalf("env garbage not defaulted: %+v", got)
		}
		out := logs.String()
		if !strings.Contains(out, "reason=not_integer") || !strings.Contains(out, "reason=not_number") {
			t.Fatalf("missing reasons: %s", out)
		}
	})
	t.Run("YAML also invalid falls to code default not YAML", func(t *testing.T) {
		captureLogs(t)
		t.Setenv("CWA_MAX_ATTEMPTS", "9")
		c := &Config{CWA: CWAConfig{MaxAttempts: 4}} // YAML 合法值 4，但環境變數無效.
		loadLike(c)
		if got := c.CWASettings().MaxAttempts; got != 2 {
			t.Fatalf("MaxAttempts = %d, want code default 2 (not YAML 4)", got)
		}
	})
	t.Run("other fields untouched", func(t *testing.T) {
		captureLogs(t)
		c := &Config{CWA: CWAConfig{SafetyRatio: 1.5, DailyLimit: 700}}
		loadLike(c)
		got := c.CWASettings()
		if got.DailyLimit != 700 || got.SafetyRatio != 0.95 {
			t.Fatalf("collateral reset: %+v", got)
		}
	})
}

// F30-36.
func TestZeroValuesMeanDefaults(t *testing.T) {
	logs := captureLogs(t)
	t.Setenv("CWA_TSUNAMI_CRON", "")
	t.Setenv("CWA_DAILY_LIMIT", "")
	c := &Config{}
	if got := c.CWASettings(); !reflect.DeepEqual(got, defaultSettings()) {
		t.Fatalf("literal Config{}: %+v", got)
	}
	loadLike(c)
	if got := c.CWASettings(); !reflect.DeepEqual(got, defaultSettings()) {
		t.Fatalf("after Load-like: %+v", got)
	}
	if strings.Contains(logs.String(), "invalid CWA setting") {
		t.Fatalf("zero values warned: %s", logs)
	}
	// 零值不是停用：四個 cron 都不是 off.
	got := c.CWASettings()
	for _, v := range []string{got.WarningCron, got.EarthquakeCron, got.TsunamiCron, got.ForecastCron} {
		if v == CronOff || v == "" {
			t.Fatalf("zero value became %q", v)
		}
	}
	// CWASettings 不改呼叫端的 Config，也不寫 log.
	bad := &Config{CWA: CWAConfig{SafetyRatio: 9}}
	logs.Reset()
	_ = bad.CWASettings()
	if bad.CWA.SafetyRatio != 9 || logs.Len() != 0 {
		t.Fatalf("CWASettings must be pure, ratio=%v logs=%q", bad.CWA.SafetyRatio, logs)
	}
}

// F30-36.
func TestCronValidityRules(t *testing.T) {
	invalid := []string{"@daily", "@every 5m", "@hourly", "* * * * * *", "0 0 31 2 *", "* * * *", "not a cron", " OFF", "OFF", "off "}
	for _, spec := range invalid {
		t.Run("invalid "+spec, func(t *testing.T) {
			logs := captureLogs(t)
			c := &Config{Alert: AlertConfig{TsunamiCron: spec}}
			loadLike(c)
			if got := c.CWASettings().TsunamiCron; got != "*/30 * * * *" {
				t.Fatalf("%q accepted, TsunamiCron = %q", spec, got)
			}
			if strings.Count(logs.String(), "invalid CWA setting") != 1 {
				t.Fatalf("want exactly one warn: %s", logs)
			}
		})
	}
	for _, spec := range []string{"*/10 * * * *", "40 5,11,17,23 * * *", "0 9 * * 1-5"} {
		t.Run("valid "+spec, func(t *testing.T) {
			logs := captureLogs(t)
			c := &Config{Alert: AlertConfig{TsunamiCron: spec}}
			loadLike(c)
			if got := c.CWASettings().TsunamiCron; got != spec || logs.Len() != 0 {
				t.Fatalf("%q rejected: got %q logs %s", spec, got, logs)
			}
		})
	}
	if _, err := ParseCWACron("0 9 * * *"); err != nil {
		t.Fatalf("ParseCWACron: %v", err)
	}
	// 以台灣時間解析：09:00 的下一次觸發換算 Taipei 必為 9 點.
	sched, err := ParseCWACron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	taipei, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	if h := sched.Next(time.Now()).In(taipei).Hour(); h != 9 {
		t.Fatalf("next trigger hour in Taipei = %d", h)
	}
}

// readProductionConfig 直接解析 configs/production.yaml（不經 Load，免機密環境變數）.
func readProductionConfig(t *testing.T) *Config {
	t.Helper()
	data, err := os.ReadFile("../../../configs/production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		t.Fatal(err)
	}
	return c
}

// F30-37：production.yaml 的預報排程為台灣時間發布後 10 分鐘.
func TestProductionWeatherCron(t *testing.T) {
	c := readProductionConfig(t)
	if c.Weather.Cron != "40 5,11,17,23 * * *" {
		t.Fatalf("weather.cron = %q", c.Weather.Cron)
	}
	if got := c.CWASettings().ForecastCron; got != "40 5,11,17,23 * * *" {
		t.Fatalf("ForecastCron = %q", got)
	}
	sched, err := ParseCWACron(c.Weather.Cron)
	if err != nil {
		t.Fatal(err)
	}
	taipei, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, taipei)
	var hours []int
	for range 5 {
		at = sched.Next(at)
		if at.In(taipei).Minute() != 40 {
			t.Fatalf("trigger %v is not at minute 40 Taipei time", at.In(taipei))
		}
		hours = append(hours, at.In(taipei).Hour())
	}
	if !reflect.DeepEqual(hours, []int{5, 11, 17, 23, 5}) {
		t.Fatalf("hours = %v", hours)
	}
}

// F30-37：production.yaml 的氣象署設定等於程式碼預設，且 YAML 本身不產生任何無效值 Warn.
func TestProductionCWASettingsAreDefaults(t *testing.T) {
	logs := captureLogs(t)
	c := readProductionConfig(t)
	applyCWAEnv(c, func(string) (string, bool) { return "", false })
	sanitizeCWA(c)
	if strings.Contains(logs.String(), "invalid CWA setting") {
		t.Fatalf("production.yaml has invalid CWA values: %s", logs)
	}
	if got := c.CWASettings(); !reflect.DeepEqual(got, defaultSettings()) {
		t.Fatalf("production CWA settings differ from defaults: %+v", got)
	}
	if c.Alert.CheckTimeoutSeconds != 120 {
		t.Fatalf("alert.check_timeout_seconds = %d, want 120", c.Alert.CheckTimeoutSeconds)
	}
	raw, err := os.ReadFile("../../../configs/production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\n  cron: \"*/5") || regexp.MustCompile(`(?m)^alert:\n(?:.*\n)*?  cron:`).Match(raw) {
		t.Fatal("alert.cron key must be removed")
	}
}
