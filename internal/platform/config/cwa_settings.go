package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	// 內嵌時區資料：映像缺 tzdata 時 Asia/Taipei 仍查得到（F30-37，規格 T12）.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// CWAConfig 氣象署共用層設定（F30-36）：重試與額度.
type CWAConfig struct {
	MaxAttempts          int     `yaml:"max_attempts"`
	DailyLimit           int     `yaml:"daily_limit"`
	SafetyRatio          float64 `yaml:"safety_ratio"`
	WarnRatio            float64 `yaml:"warn_ratio"`
	ExhaustedStatusCodes string  `yaml:"exhausted_status_codes"` // 逗號分隔整數，與環境變數同格式.
}

// CronOff 字面值：該排程不註冊. 只有完全相等才算，大小寫變體與前後空白都不算.
const CronOff = "off"

// 程式碼內的預設值（零值 / 空字串 / 無效值都換成這些）.
const (
	defaultWarningCron           = "*/5 * * * *"
	defaultEarthquakeCron        = "*/5 * * * *"
	defaultTsunamiCron           = "*/30 * * * *"
	defaultForecastCron          = "40 5,11,17,23 * * *"
	defaultColdIntervalMin       = 30
	defaultEarthquakeLimit       = 5
	defaultMaxAttempts           = 2
	defaultDailyLimit            = 1000
	defaultSafetyRatio           = 0.95
	defaultWarnRatio             = 0.8
	cwaCronLocationPrefix        = "CRON_TZ=Asia/Taipei "
	cronFieldCount               = 5
	minColdIntervalMin           = 5
	maxColdIntervalMin           = 1440
	maxEarthquakeLimit           = 50
	maxMaxAttempts               = 5
	minStatusCode, maxStatusCode = 400, 599
)

var defaultExhaustedStatus = []int{429}

// CWASettings 解析並驗證後的有型別設定. 由 (*Config).CWASettings 產生.
type CWASettings struct {
	WarningCron, EarthquakeCron, TsunamiCron, ForecastCron string // 恰 5 欄的 cron，或字面 CronOff.
	ColdInterval                                           time.Duration
	EarthquakeLimit                                        int
	MaxAttempts                                            int
	DailyLimit                                             int
	SafetyRatio                                            float64
	WarnRatio                                              float64
	ExhaustedStatus                                        []int
}

// 設定檢查的固定理由詞彙.
const (
	reasonNotInteger      = "not_integer"
	reasonNotNumber       = "not_number"
	reasonOutOfRange      = "out_of_range"
	reasonBadCron         = "bad_cron"
	reasonWarnAboveSafety = "warn_above_safety"
	reasonBadStatus       = "bad_status"
)

// ParseCWACron 以與 gocron 相同的解析器（robfig ParseStandard + CRON_TZ=Asia/Taipei 前綴）解析 cron（F30-36）.
// 非恰 5 欄、@ 描述符、沒有未來觸發時刻都回錯.
func ParseCWACron(spec string) (cron.Schedule, error) {
	if strings.HasPrefix(strings.TrimSpace(spec), "@") {
		return nil, fmt.Errorf("cron descriptors are not supported")
	}
	if n := len(strings.Fields(spec)); n != cronFieldCount {
		return nil, fmt.Errorf("cron must have exactly %d fields, got %d", cronFieldCount, n)
	}
	sched, err := cron.ParseStandard(cwaCronLocationPrefix + spec)
	if err != nil {
		return nil, fmt.Errorf("parse cron: %w", err)
	}
	if sched.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("cron has no future trigger time")
	}
	return sched, nil
}

// cronReason 檢查 cron 欄位；空字串（零值）與 off 都有效.
func cronReason(v string) string {
	if v == "" || v == CronOff {
		return ""
	}
	if _, err := ParseCWACron(v); err != nil {
		return reasonBadCron
	}
	return ""
}

func intRangeReason(v, lo, hi int) string {
	if v == 0 {
		return ""
	}
	if v < lo || v > hi {
		return reasonOutOfRange
	}
	return ""
}

func ratioReason(v, hi float64) string {
	if v == 0 {
		return ""
	}
	if math.IsNaN(v) || v <= 0 || v > hi {
		return reasonOutOfRange
	}
	return ""
}

// parseStatusCodes 解析逗號分隔的狀態碼；空字串回 (nil, true).
func parseStatusCodes(s string) ([]int, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, true
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < minStatusCode || n > maxStatusCode {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func statusReason(s string) string {
	if _, ok := parseStatusCodes(s); !ok {
		return reasonBadStatus
	}
	return ""
}

// cwaField 一個可檢查、可歸零的設定欄位.
type cwaField struct {
	key    string
	reason func(c *Config) string
	zero   func(c *Config)
}

// effectiveSafety 驗證後有效的 safety_ratio（無效 / 零值 → 預設）.
func effectiveSafety(c *Config) float64 {
	if ratioReason(c.CWA.SafetyRatio, 1) != "" || c.CWA.SafetyRatio == 0 {
		return defaultSafetyRatio
	}
	return c.CWA.SafetyRatio
}

// cwaFields 欄位清單；safety_ratio 必須排在 warn_ratio 之前.
func cwaFields() []cwaField {
	return []cwaField{
		{
			"alert.warning_cron", func(c *Config) string { return cronReason(c.Alert.WarningCron) },
			func(c *Config) { c.Alert.WarningCron = "" },
		},
		{
			"alert.earthquake_cron", func(c *Config) string { return cronReason(c.Alert.EarthquakeCron) },
			func(c *Config) { c.Alert.EarthquakeCron = "" },
		},
		{
			"alert.tsunami_cron", func(c *Config) string { return cronReason(c.Alert.TsunamiCron) },
			func(c *Config) { c.Alert.TsunamiCron = "" },
		},
		{
			"weather.cron", func(c *Config) string { return cronReason(c.Weather.Cron) },
			func(c *Config) { c.Weather.Cron = "" },
		},
		{
			"alert.cold_interval_minutes",
			func(c *Config) string {
				return intRangeReason(c.Alert.ColdIntervalMinutes, minColdIntervalMin, maxColdIntervalMin)
			},
			func(c *Config) { c.Alert.ColdIntervalMinutes = 0 },
		},
		{
			"alert.earthquake_limit",
			func(c *Config) string { return intRangeReason(c.Alert.EarthquakeLimit, 1, maxEarthquakeLimit) },
			func(c *Config) { c.Alert.EarthquakeLimit = 0 },
		},
		{
			"cwa.max_attempts",
			func(c *Config) string { return intRangeReason(c.CWA.MaxAttempts, 1, maxMaxAttempts) },
			func(c *Config) { c.CWA.MaxAttempts = 0 },
		},
		{
			"cwa.daily_limit",
			func(c *Config) string { return intRangeReason(c.CWA.DailyLimit, 1, math.MaxInt32) },
			func(c *Config) { c.CWA.DailyLimit = 0 },
		},
		{
			"cwa.safety_ratio", func(c *Config) string { return ratioReason(c.CWA.SafetyRatio, 1) },
			func(c *Config) { c.CWA.SafetyRatio = 0 },
		},
		{
			"cwa.warn_ratio",
			func(c *Config) string {
				if r := ratioReason(c.CWA.WarnRatio, 1); r != "" {
					return r
				}
				if c.CWA.WarnRatio > effectiveSafety(c) {
					return reasonWarnAboveSafety
				}
				return ""
			},
			func(c *Config) { c.CWA.WarnRatio = 0 },
		},
		{
			"cwa.exhausted_status_codes", func(c *Config) string { return statusReason(c.CWA.ExhaustedStatusCodes) },
			func(c *Config) { c.CWA.ExhaustedStatusCodes = "" },
		},
	}
}

// cwaEnvVars 環境變數對應（規格 T22）.
var cwaEnvStringVars = []struct {
	name string
	set  func(c *Config, v string)
}{
	{"CWA_WARNING_CRON", func(c *Config, v string) { c.Alert.WarningCron = v }},
	{"CWA_EARTHQUAKE_CRON", func(c *Config, v string) { c.Alert.EarthquakeCron = v }},
	{"CWA_TSUNAMI_CRON", func(c *Config, v string) { c.Alert.TsunamiCron = v }},
	{"CWA_FORECAST_CRON", func(c *Config, v string) { c.Weather.Cron = v }},
	{"CWA_EXHAUSTED_STATUS", func(c *Config, v string) { c.CWA.ExhaustedStatusCodes = v }},
}

var cwaEnvIntVars = []struct {
	name, key string
	set       func(c *Config, v int)
}{
	{"CWA_COLD_INTERVAL_MINUTES", "alert.cold_interval_minutes", func(c *Config, v int) { c.Alert.ColdIntervalMinutes = v }},
	{"CWA_EARTHQUAKE_LIMIT", "alert.earthquake_limit", func(c *Config, v int) { c.Alert.EarthquakeLimit = v }},
	{"CWA_MAX_ATTEMPTS", "cwa.max_attempts", func(c *Config, v int) { c.CWA.MaxAttempts = v }},
	{"CWA_DAILY_LIMIT", "cwa.daily_limit", func(c *Config, v int) { c.CWA.DailyLimit = v }},
}

var cwaEnvFloatVars = []struct {
	name, key string
	set       func(c *Config, v float64)
}{
	{"CWA_SAFETY_RATIO", "cwa.safety_ratio", func(c *Config, v float64) { c.CWA.SafetyRatio = v }},
	{"CWA_WARN_RATIO", "cwa.warn_ratio", func(c *Config, v float64) { c.CWA.WarnRatio = v }},
}

func warnInvalidCWA(field, reason string) {
	slog.Warn("invalid CWA setting, using default", "field", field, "reason", reason)
}

// applyCWAEnv 以環境變數覆蓋 YAML 值（非空才覆蓋）. 數值解析失敗算無效：欄位歸零並 Warn.
func applyCWAEnv(c *Config, lookup func(string) (string, bool)) {
	for _, e := range cwaEnvStringVars {
		if v, ok := lookup(e.name); ok && v != "" {
			e.set(c, v)
		}
	}
	for _, e := range cwaEnvIntVars {
		if v, ok := lookup(e.name); ok && v != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				warnInvalidCWA(e.key, reasonNotInteger)
				e.set(c, 0)
				continue
			}
			e.set(c, n)
		}
	}
	for _, e := range cwaEnvFloatVars {
		if v, ok := lookup(e.name); ok && v != "" {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				warnInvalidCWA(e.key, reasonNotNumber)
				e.set(c, 0)
				continue
			}
			e.set(c, f)
		}
	}
}

// sanitizeCWA 逐欄驗證；無效欄位 Warn 一行並歸零（之後由 CWASettings 換成程式碼預設）.
func sanitizeCWA(c *Config) {
	for _, f := range cwaFields() {
		if reason := f.reason(c); reason != "" {
			warnInvalidCWA(f.key, reason)
			f.zero(c)
		}
	}
}

// applyCWASettings 在 Load 中呼叫：環境變數覆蓋 + 逐欄驗證. 不回錯.
func applyCWASettings(c *Config) {
	applyCWAEnv(c, os.LookupEnv)
	sanitizeCWA(c)
}

// CWASettings 產生有型別的氣象署設定（F30-36）. 純函式：不讀環境變數、不寫 log；
// 零值 / 空字串 / 無效值一律換成程式碼預設，所以對字面量 Config{} 也成立.
func (c *Config) CWASettings() CWASettings {
	// 複製一份，把無效欄位歸零後再補預設，不改到呼叫端的 Config.
	cp := *c
	for _, f := range cwaFields() {
		if f.reason(&cp) != "" {
			f.zero(&cp)
		}
	}
	s := CWASettings{
		WarningCron:     orDefault(cp.Alert.WarningCron, defaultWarningCron),
		EarthquakeCron:  orDefault(cp.Alert.EarthquakeCron, defaultEarthquakeCron),
		TsunamiCron:     orDefault(cp.Alert.TsunamiCron, defaultTsunamiCron),
		ForecastCron:    orDefault(cp.Weather.Cron, defaultForecastCron),
		ColdInterval:    time.Duration(intOrDefault(cp.Alert.ColdIntervalMinutes, defaultColdIntervalMin)) * time.Minute,
		EarthquakeLimit: intOrDefault(cp.Alert.EarthquakeLimit, defaultEarthquakeLimit),
		MaxAttempts:     intOrDefault(cp.CWA.MaxAttempts, defaultMaxAttempts),
		DailyLimit:      intOrDefault(cp.CWA.DailyLimit, defaultDailyLimit),
		SafetyRatio:     defaultSafetyRatio,
		WarnRatio:       defaultWarnRatio,
	}
	if cp.CWA.SafetyRatio != 0 {
		s.SafetyRatio = cp.CWA.SafetyRatio
	}
	if cp.CWA.WarnRatio != 0 {
		s.WarnRatio = cp.CWA.WarnRatio
	}
	if codes, _ := parseStatusCodes(cp.CWA.ExhaustedStatusCodes); len(codes) > 0 {
		s.ExhaustedStatus = codes
	} else {
		s.ExhaustedStatus = append([]int(nil), defaultExhaustedStatus...)
	}
	return s
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func intOrDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
