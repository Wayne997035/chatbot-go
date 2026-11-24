package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
)

// [F37-02] AI 關閉時，單打地標落到最後一步的固定句整句全等，並教「地標＋天氣」.
func TestNoAssistantFallbackTeachesLandmarkPlusWeather(t *testing.T) {
	for _, text := range []string{"日月潭", "火星"} {
		t.Run(text, func(t *testing.T) {
			api := newLineAPI(t, nil)
			h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
			h.handleTextMessage(context.Background(), "U1", text, "tok")

			want := "找不到「" + text + "」的天氣資料，請輸入區域名稱（如：大安區、天母；地標請加「天氣」，如：日月潭天氣），或直接傳送位置"
			reply := api.lastReply(t)
			if !strings.Contains(reply, want) {
				t.Errorf("reply = %s\nwant containing %q", reply, want)
			}
			if n := strings.Count(reply, "地標請加「天氣」"); n != 1 {
				t.Errorf("landmark hint appears %d times, want exactly 1", n)
			}
		})
	}
}
