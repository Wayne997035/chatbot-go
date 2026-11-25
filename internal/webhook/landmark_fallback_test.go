package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
)

// [F38-04] AI 關閉時，查不到的單打詞落到最後一步的固定句整句全等：地名或景點可直接打（天母、日月潭），查不到才加「天氣」.
func TestNoAssistantFallbackNamesDirectPlacesAndWeatherWord(t *testing.T) {
	for _, text := range []string{"日月潭", "火星"} {
		t.Run(text, func(t *testing.T) {
			api := newLineAPI(t, nil)
			h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
			h.handleTextMessage(context.Background(), "U1", text, "tok")

			want := "找不到「" + text + "」的天氣資料，請直接打地名或景點（如：大安區、天母、日月潭），查不到請在後面加「天氣」，或直接傳送位置"
			reply := api.lastReply(t)
			if !strings.Contains(reply, want) {
				t.Errorf("reply = %s\nwant containing %q", reply, want)
			}
			if n := strings.Count(reply, "加「天氣」"); n != 1 {
				t.Errorf("weather-word hint appears %d times, want exactly 1", n)
			}
			if strings.Contains(reply, "地標請加") {
				t.Errorf("old landmark-needs-weather-word wording must be gone: %s", reply)
			}
		})
	}
}
