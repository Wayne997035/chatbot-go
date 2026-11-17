package app

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
	"reflect"
	"testing"
	"time"
)

// F33-11：AIConfig → ai.Settings 的映射；key 與 ai 常數一致、秒數轉 Duration、順序沿用 SplitList.
func TestAISettingsMapping(t *testing.T) {
	full := &config.AIConfig{
		ProviderOrder: "Groq,gemini", VisionProviderOrder: "gemini",
		NVIDIAModels: "Nvidia/Foo-1,b", GroqModels: "g1", GeminiModels: "gem-1,gem-2",
		AttemptTimeoutSeconds: "5", ProviderBudgetSeconds: "10", ProviderCooldownSeconds: "30", RateLimitWaitSeconds: "8",
	}
	tests := []struct {
		name string
		in   *config.AIConfig
		want ai.Settings
	}{
		{"full", full, ai.Settings{
			TextOrder: []string{"groq", "gemini"}, VisionOrder: []string{"gemini"},
			Models: map[string][]string{
				ai.ProviderNVIDIA: {"Nvidia/Foo-1", "b"}, ai.ProviderGroq: {"g1"}, ai.ProviderGemini: {"gem-1", "gem-2"},
			},
			AttemptTimeout: 5 * time.Second, ProviderBudget: 10 * time.Second,
			ProviderCooldown: 30 * time.Second, RateLimitWait: 8 * time.Second,
		}},
		{"empty", &config.AIConfig{}, ai.Settings{Models: map[string][]string{}}},
		{"invalid values act as unset", &config.AIConfig{NVIDIAModels: "a b", AttemptTimeoutSeconds: "abc"}, ai.Settings{Models: map[string][]string{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := aiSettings(tc.in)
			if len(got.TextOrder) == 0 {
				got.TextOrder = nil
			}
			if len(got.VisionOrder) == 0 {
				got.VisionOrder = nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("aiSettings = %+v, want %+v", got, tc.want)
			}
		})
	}
}
