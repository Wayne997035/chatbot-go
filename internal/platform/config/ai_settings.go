package config

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AI 供應商代號；config 不 import internal/ai（避免循環與雙重預設），
// 所以用字面量，由 app 的 TestAISettingsMapping 鎖住與 ai 常數一致.
const (
	aiKeyNVIDIA = "nvidia"
	aiKeyGroq   = "groq"
	aiKeyGemini = "gemini"
)

// maxAISeconds [F33-08] 秒數上限（一小時）；遠大於實際值（8/10/60），只擋手滑多打的位數.
const maxAISeconds = 3600

// modelNamePattern [F33-09] 模型 id 字元白名單；id 會被拼進 Gemini URL path，不得夾帶 ? # 空白等.
var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// AISettings [F33-08] 有型別的 AI 路由設定；零值 = 未設或無效，由 ai 套件補預設.
type AISettings struct {
	Models           map[string][]string // key: "nvidia" / "groq" / "gemini",值已過 SplitModels
	AttemptTimeout   time.Duration
	ProviderBudget   time.Duration
	ProviderCooldown time.Duration
	RateLimitWait    time.Duration
}

// aiSecondsField [F33-08] 一個秒數欄位：yaml key、欄位指標、0 是否合法.
type aiSecondsField struct {
	key       string
	ptr       func(*AIConfig) *string
	zeroValid bool
}

func aiSecondsFields() []aiSecondsField {
	return []aiSecondsField{
		{"attempt_timeout_seconds", func(a *AIConfig) *string { return &a.AttemptTimeoutSeconds }, false},
		{"provider_budget_seconds", func(a *AIConfig) *string { return &a.ProviderBudgetSeconds }, false},
		{"provider_cooldown_seconds", func(a *AIConfig) *string { return &a.ProviderCooldownSeconds }, false},
		// 0 = 不原地等待、直接換下一個模型（A3）.
		{"rate_limit_wait_seconds", func(a *AIConfig) *string { return &a.RateLimitWaitSeconds }, true},
	}
}

// parseAISeconds [F33-08] 回傳秒數與無效原因（"" = 合法或未設）；空字串 = 未設.
func parseAISeconds(v string, zeroValid bool) (secs int, reason string) {
	if v == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, "not_integer"
	}
	if n < 0 || n > maxAISeconds || (n == 0 && !zeroValid) {
		return 0, "out_of_range"
	}
	return n, ""
}

type aiModelsField struct {
	key string // yaml key
	ptr func(*AIConfig) *string
}

func aiModelsFields() []aiModelsField {
	return []aiModelsField{
		{"nvidia_models", func(a *AIConfig) *string { return &a.NVIDIAModels }},
		{"groq_models", func(a *AIConfig) *string { return &a.GroqModels }},
		{"gemini_models", func(a *AIConfig) *string { return &a.GeminiModels }},
	}
}

// splitModels 切清單並回傳被丟棄項目的序號（從 0 算起，含空項以外的所有不合格與重複以外者）.
func splitModels(value string) (models []string, droppedIdx []int) {
	seen := map[string]bool{}
	for i, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !modelNamePattern.MatchString(part) || strings.Contains(part, "..") {
			droppedIdx = append(droppedIdx, i)
			continue
		}
		if seen[part] {
			continue
		}
		seen[part] = true
		models = append(models, part)
	}
	return models, droppedIdx
}

// SplitModels [F33-09] 把逗號分隔的模型清單切開：去空白、略過空項、保留大小寫
// （NEVER 用 SplitList，它會小寫化，破壞區分大小寫的模型 id）、去重保首次；
// 不符字元白名單的項目丟棄並計入 dropped（內容不外流到 log）.
func SplitModels(value string) (models []string, dropped int) {
	models, idx := splitModels(value)
	return models, len(idx)
}

// sanitizeAI [F33-08] 逐欄驗證 AI 設定；無效欄位清成 "" 並 warn 一行（不含值，可能是誤貼的金鑰），不讓 Load 失敗.
func sanitizeAI(c *Config) {
	a := &c.AI
	for _, f := range aiSecondsFields() {
		if _, reason := parseAISeconds(*f.ptr(a), f.zeroValid); reason != "" {
			slog.Warn("invalid AI setting, using default", "field", "ai."+f.key, "reason", reason)
			*f.ptr(a) = ""
		}
	}
	for _, f := range aiModelsFields() {
		raw := f.ptr(a)
		models, idx := splitModels(*raw)
		for _, i := range idx {
			slog.Warn("invalid AI setting, using default", "field", "ai."+f.key, "index", i, "reason", "bad_model_name")
		}
		if len(models) == 0 && *raw != "" {
			*raw = "" // 全部被丟 = 自動排名
		}
	}
}

// Settings [F33-08] 產生有型別的 AI 設定. 純函式：不讀環境變數、不寫 log；無效值當未設.
func (c *AIConfig) Settings() AISettings {
	s := AISettings{Models: map[string][]string{}}
	for key, raw := range map[string]string{aiKeyNVIDIA: c.NVIDIAModels, aiKeyGroq: c.GroqModels, aiKeyGemini: c.GeminiModels} {
		if models, _ := SplitModels(raw); len(models) > 0 {
			s.Models[key] = models
		}
	}
	dur := func(v string, zeroValid bool) time.Duration {
		n, _ := parseAISeconds(v, zeroValid)
		return time.Duration(n) * time.Second
	}
	s.AttemptTimeout = dur(c.AttemptTimeoutSeconds, false)
	s.ProviderBudget = dur(c.ProviderBudgetSeconds, false)
	s.ProviderCooldown = dur(c.ProviderCooldownSeconds, false)
	s.RateLimitWait = dur(c.RateLimitWaitSeconds, true)
	return s
}
