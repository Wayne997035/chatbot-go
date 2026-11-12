package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"time"
)

// testCWAClient 指向 baseURL 的氣象署客戶端：預設設定、記憶體記帳、重試等待縮短.
func testCWAClient(baseURL string) *cwa.Client {
	settings := (&config.Config{}).CWASettings()
	return cwa.NewClient(cwa.ClientOptions{
		BaseURL: baseURL, AuthKey: "test-key", Backoff: time.Millisecond,
	}, cwa.NewLedger(nil, settings, nil, time.Now), settings)
}
