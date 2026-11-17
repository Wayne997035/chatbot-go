package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// aiLines 回傳擷取輸出中含 "invalid AI setting" 的行（Load 會另印其他 warn，不計入）.
func aiLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "invalid AI setting") {
			lines = append(lines, l)
		}
	}
	return lines
}

// noTime 去掉每行的 time=... 欄位，避免輸入值（如 "-1"）與時間戳字樣碰巧相同而誤判.
func noTime(out string) string { return timeAttr.ReplaceAllString(out, "") }

var timeAttr = regexp.MustCompile(`time=\S+ `)

// 不得 t.Parallel：slog.SetDefault 是全域.

// F33-08：七個變數全未設 → 全零、無 warn；只設一個 → 只有該欄非零.
func TestAISettingsUnsetIsSilentDefault(t *testing.T) {
	logs := captureLogs(t)
	c := &Config{}
	sanitizeAI(c)
	s := c.AI.Settings()
	if len(s.Models) != 0 || s.AttemptTimeout != 0 || s.ProviderBudget != 0 || s.ProviderCooldown != 0 || s.RateLimitWait != 0 {
		t.Errorf("unset settings = %+v, want all zero", s)
	}
	if got := aiLines(logs.String()); len(got) != 0 {
		t.Errorf("unset must be silent, got %v", got)
	}

	c = &Config{AI: AIConfig{AttemptTimeoutSeconds: "5"}}
	sanitizeAI(c)
	s = c.AI.Settings()
	if s.AttemptTimeout != 5*time.Second || s.ProviderBudget != 0 || s.ProviderCooldown != 0 || s.RateLimitWait != 0 || len(s.Models) != 0 {
		t.Errorf("only attempt should be set, got %+v", s)
	}
}

// F33-08：合法值轉成對應秒數.
func TestAISettingsValidValues(t *testing.T) {
	logs := captureLogs(t)
	c := &Config{AI: AIConfig{
		AttemptTimeoutSeconds: "5", ProviderBudgetSeconds: " 10 ", ProviderCooldownSeconds: "30", RateLimitWaitSeconds: "8",
	}}
	sanitizeAI(c)
	s := c.AI.Settings()
	if s.AttemptTimeout != 5*time.Second || s.ProviderBudget != 10*time.Second || s.ProviderCooldown != 30*time.Second || s.RateLimitWait != 8*time.Second {
		t.Errorf("settings = %+v", s)
	}
	c = &Config{AI: AIConfig{RateLimitWaitSeconds: "0"}}
	sanitizeAI(c)
	if c.AI.RateLimitWaitSeconds != "0" || c.AI.Settings().RateLimitWait != 0 {
		t.Errorf("rate_limit_wait 0 must stay valid, got %q", c.AI.RateLimitWaitSeconds)
	}
	if got := aiLines(logs.String()); len(got) != 0 {
		t.Errorf("valid values must not warn, got %v", got)
	}
}

// F33-08：0 只對 rate_limit_wait 合法（A3）.
func TestZeroOnlyValidForRateLimitWait(t *testing.T) {
	tests := []struct {
		name      string
		cfg       AIConfig
		wantWarns int
		wantField string
	}{
		{"attempt", AIConfig{AttemptTimeoutSeconds: "0"}, 1, "ai.attempt_timeout_seconds"},
		{"budget", AIConfig{ProviderBudgetSeconds: "0"}, 1, "ai.provider_budget_seconds"},
		{"cooldown", AIConfig{ProviderCooldownSeconds: "0"}, 1, "ai.provider_cooldown_seconds"},
		{"rate_limit_wait", AIConfig{RateLimitWaitSeconds: "0"}, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			c := &Config{AI: tc.cfg}
			sanitizeAI(c)
			lines := aiLines(logs.String())
			if len(lines) != tc.wantWarns {
				t.Fatalf("warn lines = %d (%v), want %d", len(lines), lines, tc.wantWarns)
			}
			if tc.wantWarns == 1 && (!strings.Contains(lines[0], "field="+tc.wantField) || !strings.Contains(lines[0], "reason=out_of_range")) {
				t.Errorf("warn = %s", lines[0])
			}
		})
	}
}

// F33-08：非法值歸零、每欄剛好 1 行 warn、不含輸入值.
func TestAISettingsInvalidFallsBackAndWarns(t *testing.T) {
	type fld struct {
		key string
		set func(*AIConfig, string)
		get func(*AIConfig) string
	}
	fields := []fld{
		{"attempt_timeout_seconds", func(a *AIConfig, v string) { a.AttemptTimeoutSeconds = v }, func(a *AIConfig) string { return a.AttemptTimeoutSeconds }},
		{"provider_budget_seconds", func(a *AIConfig, v string) { a.ProviderBudgetSeconds = v }, func(a *AIConfig) string { return a.ProviderBudgetSeconds }},
		{"provider_cooldown_seconds", func(a *AIConfig, v string) { a.ProviderCooldownSeconds = v }, func(a *AIConfig) string { return a.ProviderCooldownSeconds }},
		{"rate_limit_wait_seconds", func(a *AIConfig, v string) { a.RateLimitWaitSeconds = v }, func(a *AIConfig) string { return a.RateLimitWaitSeconds }},
	}
	inputs := []struct{ val, reason, leak string }{
		{"-1", "out_of_range", "-1"},
		{"abc", "not_integer", "abc"},
		{"  ", "not_integer", ""},
		{"1.5", "not_integer", "1.5"},
		{"99999999999999999999", "not_integer", "99999999999999999999"},
		{"3601", "out_of_range", "3601"},
	}
	for _, f := range fields {
		for _, in := range inputs {
			t.Run(f.key+"/"+in.val, func(t *testing.T) {
				logs := captureLogs(t)
				c := &Config{}
				f.set(&c.AI, in.val)
				sanitizeAI(c)
				if got := f.get(&c.AI); got != "" {
					t.Errorf("field = %q, want cleared", got)
				}
				lines := aiLines(logs.String())
				if len(lines) != 1 {
					t.Fatalf("warn lines = %d (%v), want 1", len(lines), lines)
				}
				if !strings.Contains(lines[0], "field=ai."+f.key) || !strings.Contains(lines[0], "reason="+in.reason) {
					t.Errorf("warn = %s", lines[0])
				}
				if in.leak != "" && strings.Contains(noTime(logs.String()), in.leak) {
					t.Errorf("warn leaks input %q: %s", in.leak, logs.String())
				}
				if d := c.AI.Settings(); d.AttemptTimeout+d.ProviderBudget+d.ProviderCooldown+d.RateLimitWait != 0 {
					t.Errorf("settings = %+v, want zero", d)
				}
			})
		}
	}
	// 合法值與空字串：0 行 warn.
	logs := captureLogs(t)
	c := &Config{AI: AIConfig{AttemptTimeoutSeconds: "5", ProviderBudgetSeconds: ""}}
	sanitizeAI(c)
	if got := aiLines(logs.String()); len(got) != 0 {
		t.Errorf("valid/empty warned: %v", got)
	}
}

// F33-09.
func TestSplitModelsPreservesCaseDedupsAndRejectsBadNames(t *testing.T) {
	got, dropped := SplitModels(" Nvidia/Foo-1 ,, groq/bar ,Nvidia/Foo-1")
	if !reflect.DeepEqual(got, []string{"Nvidia/Foo-1", "groq/bar"}) || dropped != 0 {
		t.Errorf("got %v dropped %d", got, dropped)
	}
	tests := []struct {
		name, in string
		want     []string
		dropped  int
	}{
		{"space inside", "a b,ok", []string{"ok"}, 1},
		{"query chars", "x?y=z,ok", []string{"ok"}, 1},
		{"path traversal", "../x,ok", []string{"ok"}, 1},
		{"dotdot inside", "a..b,ok", []string{"ok"}, 1},
		{"hash", "a#b,ok", []string{"ok"}, 1},
		{"leading dash", "-a,ok", []string{"ok"}, 1},
		{"129 chars", strings.Repeat("a", 129) + ",ok", []string{"ok"}, 1},
		{"128 chars ok", strings.Repeat("a", 128), []string{strings.Repeat("a", 128)}, 0},
		{"all dropped", "a b,x?y", nil, 2},
		{"empty", "", nil, 0},
		{"colon dot ok", "gemini-2.5-flash:x", []string{"gemini-2.5-flash:x"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := SplitModels(tc.in)
			if !reflect.DeepEqual(got, tc.want) || dropped != tc.dropped {
				t.Errorf("SplitModels(%q) = %v, %d; want %v, %d", tc.in, got, dropped, tc.want, tc.dropped)
			}
		})
	}
}

// F33-09：warn 只記欄位、序號、reason，不含被丟項目內容；全丟 → 欄位清空.
func TestAISettingsBadModelNamesWarnWithoutValue(t *testing.T) {
	logs := captureLogs(t)
	c := &Config{AI: AIConfig{NVIDIAModels: "good/one,sk-secretkey?x=y1,../etc", GroqModels: "q#r", GeminiModels: "fine"}}
	sanitizeAI(c)
	lines := aiLines(logs.String())
	if len(lines) != 3 {
		t.Fatalf("warn lines = %d (%v), want 3", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "reason=bad_model_name") || !strings.Contains(l, "field=ai.") {
			t.Errorf("warn = %s", l)
		}
	}
	for _, leak := range []string{"secretkey", "../etc", "q#r", "y1", "x?"} {
		if strings.Contains(noTime(logs.String()), leak) {
			t.Errorf("warn leaks %q: %s", leak, logs.String())
		}
	}
	if c.AI.GroqModels != "" {
		t.Errorf("all-dropped list must be cleared, got %q", c.AI.GroqModels)
	}
	s := c.AI.Settings()
	if !reflect.DeepEqual(s.Models["nvidia"], []string{"good/one"}) || s.Models["groq"] != nil || !reflect.DeepEqual(s.Models["gemini"], []string{"fine"}) {
		t.Errorf("Models = %v", s.Models)
	}
}

// F33-10：production.yaml 的 key 與環境變數名一致.
func TestProductionYAMLMapsAIEnv(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "production.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"NVIDIA_MODELS": "v1", "GROQ_MODELS": "v2", "GEMINI_MODELS": "v3",
		"AI_ATTEMPT_TIMEOUT_SECONDS": "v4", "AI_PROVIDER_BUDGET_SECONDS": "v5",
		"AI_PROVIDER_COOLDOWN_SECONDS": "v6", "AI_RATE_LIMIT_WAIT_SECONDS": "v7",
	}
	parse := func() *Config {
		c := &Config{}
		if err := yaml.Unmarshal(data, c); err != nil {
			t.Fatalf("parse: %v", err)
		}
		expandEnvFields(reflect.ValueOf(c).Elem())
		return c
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	a := parse().AI
	got := []string{a.NVIDIAModels, a.GroqModels, a.GeminiModels, a.AttemptTimeoutSeconds, a.ProviderBudgetSeconds, a.ProviderCooldownSeconds, a.RateLimitWaitSeconds}
	if !reflect.DeepEqual(got, []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7"}) {
		t.Errorf("mapped = %v", got)
	}
	for k := range vars {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	a = parse().AI
	if (a.NVIDIAModels + a.GroqModels + a.GeminiModels + a.AttemptTimeoutSeconds + a.ProviderBudgetSeconds + a.ProviderCooldownSeconds + a.RateLimitWaitSeconds) != "" {
		t.Errorf("unset env must yield empty fields, got %+v", a)
	}
}

// F33-08 接線：經過真正的 Load()，非法值被清掉、Load 不失敗.
func TestLoadSanitizesAISettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o750); err != nil {
		t.Fatal(err)
	}
	yamlText := `database:
  mongo:
    uri: ${MONGO_URI}
line:
  channel_secret: ${LINE_CHANNEL_SECRET}
  channel_token: ${LINE_CHANNEL_TOKEN}
ai:
  nvidia_models: ${NVIDIA_MODELS}
  attempt_timeout_seconds: ${AI_ATTEMPT_TIMEOUT_SECONDS}
`
	if err := os.WriteFile(filepath.Join(dir, "configs", "testenv.yaml"), []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("APP_ENV", "testenv")
	t.Setenv("MONGO_URI", "mongodb://h")
	t.Setenv("LINE_CHANNEL_SECRET", "s")
	t.Setenv("LINE_CHANNEL_TOKEN", "t")
	t.Setenv("NVIDIA_MODELS", "Good/Model,bad name,x?y")
	t.Setenv("AI_ATTEMPT_TIMEOUT_SECONDS", "-1")
	logs := captureLogs(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load must not fail on bad AI settings: %v", err)
	}
	if c.AI.AttemptTimeoutSeconds != "" || c.AI.Settings().AttemptTimeout != 0 {
		t.Errorf("attempt = %q / %v, want cleared", c.AI.AttemptTimeoutSeconds, c.AI.Settings().AttemptTimeout)
	}
	if got := c.AI.Settings().Models["nvidia"]; !reflect.DeepEqual(got, []string{"Good/Model"}) {
		t.Errorf("nvidia models = %v", got)
	}
	if n := len(aiLines(logs.String())); n != 3 {
		t.Errorf("AI warn lines = %d, want 3 (1 seconds + 2 bad models)", n)
	}
}
