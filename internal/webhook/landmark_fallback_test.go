package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
)

// [F38-04][F38-10] AI 關閉時，查不到的單打詞落到最後一步的固定句整句全等：查不到請改打所在鄉鎮區或傳送位置，不教加「天氣」.
func TestNoAssistantFallbackNamesDirectPlacesAndWeatherWord(t *testing.T) {
	for _, text := range []string{"日月潭", "火星"} {
		t.Run(text, func(t *testing.T) {
			api := newLineAPI(t, nil)
			h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
			h.handleTextMessage(context.Background(), "U1", text, "tok")

			want := "找不到「" + text + "」的天氣資料，請改打所在鄉鎮區（如：大安區）或直接傳送位置"
			reply := api.lastReply(t)
			if !strings.Contains(reply, `"text":"`+want+`"`) {
				t.Errorf("reply = %s\nwant text exactly %q", reply, want)
			}
			if strings.Contains(reply, "再傳一次") || addWeatherWordHint.MatchString(reply) {
				t.Errorf("reply must not tell users to add the weather word: %s", reply)
			}
			if strings.Contains(reply, "地標請加") {
				t.Errorf("old landmark-needs-weather-word wording must be gone: %s", reply)
			}
		})
	}
}
