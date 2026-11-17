package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// realNVIDIAModels 決策 b2ea313f 的 NVIDIA_MODELS 真實值（逐字）.
const realNVIDIAModels = "nvidia/nemotron-3-ultra-550b-a55b,nvidia/nemotron-3.5-super-vl-120b-a12b," +
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning,nvidia/nemotron-3.5-lightning-30b-a3b,nvidia/nemotron-3.5-bolt-e4b"

var realNVIDIAList = []string{
	"nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/nemotron-3.5-super-vl-120b-a12b",
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
	"nvidia/nemotron-3.5-lightning-30b-a3b",
	"nvidia/nemotron-3.5-bolt-e4b",
}

// unsetLegacyEnv 讓這支測試不受機器上 NVIDIA_MODEL 影響（t.Setenv 註冊還原，再真的 Unsetenv）.
func unsetLegacyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NVIDIA_MODEL", "")
	if err := os.Unsetenv("NVIDIA_MODEL"); err != nil {
		t.Fatal(err)
	}
}

// [F33-33] G-02：真實整串 → 五項原樣原順序、dropped 0、設定層 0 行 warn.
func TestSplitModelsRealNVIDIAValue(t *testing.T) {
	t.Setenv("NVIDIA_MODEL", "")
	logs := captureLogs(t)
	got, dropped := SplitModels(realNVIDIAModels)
	if !reflect.DeepEqual(got, realNVIDIAList) || dropped != 0 {
		t.Errorf("SplitModels = %v, %d; want the five real names in order, 0 dropped", got, dropped)
	}
	c := &Config{AI: AIConfig{NVIDIAModels: realNVIDIAModels}}
	sanitizeAI(c)
	if logs.Len() != 0 {
		t.Errorf("real value must produce no warn, got %q", logs.String())
	}
	if m := c.AI.Settings().Models; !reflect.DeepEqual(m, map[string][]string{"nvidia": realNVIDIAList}) {
		t.Errorf("Models = %v", m)
	}
}

// [F33-34] G-04：四個秒數欄位的下界 1 與上界 3600 都合法（回滾開關 =1）.
func TestAISecondsBoundariesAreValid(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{{"1", time.Second}, {"3600", 3600 * time.Second}} {
		t.Run(tc.in, func(t *testing.T) {
			t.Setenv("NVIDIA_MODEL", "")
			logs := captureLogs(t)
			c := &Config{AI: AIConfig{
				AttemptTimeoutSeconds: tc.in, ProviderBudgetSeconds: tc.in, ProviderCooldownSeconds: tc.in, RateLimitWaitSeconds: tc.in,
			}}
			sanitizeAI(c)
			s := c.AI.Settings()
			if n := len(aiLines(logs.String())); n != 0 {
				t.Errorf("warn lines = %d, want 0: %s", n, logs.String())
			}
			if s.AttemptTimeout != tc.want || s.ProviderBudget != tc.want || s.ProviderCooldown != tc.want || s.RateLimitWait != tc.want {
				t.Errorf("Settings = %+v, want every field %v", s, tc.want)
			}
		})
	}
}

// [F33-36] G-11：全形逗號與中文被丟並記 warn（不含輸入值）.
func TestSplitModelsRejectsNonASCII(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
		dropped  int
	}{
		{"fullwidth comma", "nvidia/a\uff0cnvidia/b", nil, 1},
		{"cjk item", "模型,ok", []string{"ok"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NVIDIA_MODEL", "")
			got, dropped := SplitModels(tc.in)
			if !reflect.DeepEqual(got, tc.want) || dropped != tc.dropped {
				t.Errorf("SplitModels(%q) = %v, %d; want %v, %d", tc.in, got, dropped, tc.want, tc.dropped)
			}
			logs := captureLogs(t)
			sanitizeAI(&Config{AI: AIConfig{GroqModels: tc.in}})
			if n := len(aiLines(logs.String())); n != tc.dropped {
				t.Errorf("warn lines = %d, want %d: %s", n, tc.dropped, logs.String())
			}
			for _, leak := range []string{"模型", "nvidia/a", "\uff0c"} {
				if strings.Contains(noTime(logs.String()), leak) {
					t.Errorf("warn leaks %q: %s", leak, logs.String())
				}
			}
		})
	}
}

// [F33-37] D-02：長得像金鑰的項目丟掉並記 warn（reason=looks_like_secret，不含輸入）；只比開頭.
// 全部用假字元，NEVER 用真金鑰.
func TestSplitModelsRejectsSecretLikeNames(t *testing.T) {
	drop := map[string]string{
		"nvapi":        "nvapi-" + strings.Repeat("x", 64),
		"gsk":          "gsk_" + strings.Repeat("x", 52),
		"AIza":         "AIza" + strings.Repeat("x", 35),
		"sk-ant":       "sk-ant-api03-" + strings.Repeat("x", 90),
		"last segment": "nvidia/nvapi-" + strings.Repeat("x", 64),
	}
	for name, in := range drop {
		t.Run("drop "+name, func(t *testing.T) {
			t.Setenv("NVIDIA_MODEL", "")
			got, dropped := SplitModels(in + ",ok")
			if !reflect.DeepEqual(got, []string{"ok"}) || dropped != 1 {
				t.Errorf("got %v, %d; want [ok], 1", got, dropped)
			}
			logs := captureLogs(t)
			sanitizeAI(&Config{AI: AIConfig{NVIDIAModels: in + ",ok"}})
			lines := aiLines(logs.String())
			if len(lines) != 1 || !strings.Contains(lines[0], "reason=looks_like_secret") || !strings.Contains(lines[0], "index=0") {
				t.Errorf("warn = %v, want exactly one looks_like_secret line", lines)
			}
			if strings.Contains(noTime(logs.String()), in[:8]) {
				t.Errorf("warn leaks the value: %s", logs.String())
			}
		})
	}

	keep := append([]string{}, realNVIDIAList...)
	keep = append(keep, "deepseek-r1", "foo-gsk_bar", "nvidia/foo-nvapi-x", "NVAPI-x", "GSK_x", "aiza-x", "nvapi", "gsk-x", "sk-ant", "sk-x")
	for _, in := range keep {
		t.Run("keep "+in, func(t *testing.T) {
			got, dropped := SplitModels(in)
			if !reflect.DeepEqual(got, []string{in}) || dropped != 0 {
				t.Errorf("SplitModels(%q) = %v, %d; want kept, 0 dropped", in, got, dropped)
			}
		})
	}
}

// [F33-37] D-01：舊的單數 NVIDIA_MODEL 有值且 NVIDIA_MODELS 沒值 → 恰 1 行 warn、不含值.
func TestLegacyNVIDIAModelEnvWarns(t *testing.T) {
	legacyLines := func(out string) []string {
		var lines []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "legacy NVIDIA_MODEL") {
				lines = append(lines, l)
			}
		}
		return lines
	}
	tests := []struct {
		name   string
		setEnv bool
		env    string
		models string
		want   int
	}{
		{"legacy set, models empty", true, "nvidia/secret-x", "", 1},
		{"models set", true, "nvidia/secret-x", "nvidia/a", 0},
		{"legacy unset", false, "", "", 0},
		{"legacy only spaces", true, "   ", "", 0},
		{"models all dropped counts as empty", true, "nvidia/secret-x", "a b", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unsetLegacyEnv(t)
			if tc.setEnv {
				t.Setenv("NVIDIA_MODEL", tc.env)
			}
			logs := captureLogs(t)
			sanitizeAI(&Config{AI: AIConfig{NVIDIAModels: tc.models}})
			if got := legacyLines(logs.String()); len(got) != tc.want {
				t.Errorf("legacy warn lines = %d (%v), want %d", len(got), got, tc.want)
			}
			if strings.Contains(logs.String(), "secret-x") {
				t.Errorf("warn leaks the legacy value: %s", logs.String())
			}
		})
	}

	t.Run("through Load", func(t *testing.T) {
		unsetLegacyEnv(t)
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o750); err != nil {
			t.Fatal(err)
		}
		yamlText := "database:\n  mongo:\n    uri: ${MONGO_URI}\nline:\n  channel_secret: ${LINE_CHANNEL_SECRET}\n" +
			"  channel_token: ${LINE_CHANNEL_TOKEN}\nai:\n  nvidia_models: ${NVIDIA_MODELS}\n"
		if err := os.WriteFile(filepath.Join(dir, "configs", "testenv.yaml"), []byte(yamlText), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("APP_ENV", "testenv")
		t.Setenv("MONGO_URI", "mongodb://h")
		t.Setenv("LINE_CHANNEL_SECRET", "s")
		t.Setenv("LINE_CHANNEL_TOKEN", "t")
		t.Setenv("NVIDIA_MODELS", "")
		if err := os.Unsetenv("NVIDIA_MODELS"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("NVIDIA_MODEL", "nvidia/secret-x")
		logs := captureLogs(t)
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := legacyLines(logs.String()); len(got) != 1 {
			t.Errorf("legacy warn lines via Load = %d (%v), want 1", len(got), got)
		}
		if strings.Contains(logs.String(), "secret-x") {
			t.Errorf("warn leaks the legacy value: %s", logs.String())
		}
	})
}
