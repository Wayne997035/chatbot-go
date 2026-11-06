package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// erasedBeforeNow 回傳「刪除發生在這則訊息送出之後」的 ctx：訊息是 1 分鐘前送出，刪除標記在現在.
func erasedBeforeNow(t *testing.T, h *WebhookHandler, userID string) context.Context {
	t.Helper()
	h.erased.mark(context.Background(), userID)
	return withSentAt(context.Background(), time.Now().Add(-time.Minute).UnixMilli())
}

// TestForecastAfterErasureDoesNotRememberLocation [F24-64] 刪除前送出的查詢，刪除後才處理完，不把地點記回去.
func TestForecastAfterErasureDoesNotRememberLocation(t *testing.T) {
	forecast := &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區",
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "80"}},
	}
	tests := []struct {
		name         string
		erased       bool
		wantRemember bool
	}{
		{"no erasure remembers location", false, true},
		{"sent before erasure drops location", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newLineAPI(t, nil)
			h := newWeatherHandler(api.client, "not json")
			h.locations = weather.NewUserLocations(nil, weather.NewLookup(&fakeWeatherRepo{forecast: forecast}, nil, 60))
			ctx := context.Background()
			if tt.erased {
				ctx = erasedBeforeNow(t, h, "U1")
			}

			h.replyForecast(ctx, "U1", "tok", forecast, "")

			if reply := api.lastReply(t); !strings.Contains(reply, "信義區") {
				t.Errorf("forecast must still be replied: %s", reply)
			}
			remembered := h.locations.ForUser(context.Background(), "U1") != ""
			if remembered != tt.wantRemember {
				t.Errorf("location remembered = %v, want %v", remembered, tt.wantRemember)
			}
		})
	}
}

// memHistory 記在記憶體的問答紀錄.
type memHistory struct {
	mu    sync.Mutex
	turns []assistant.Turn
}

func (m *memHistory) Recent(context.Context, string) []assistant.Turn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]assistant.Turn(nil), m.turns...)
}

func (m *memHistory) Append(_ context.Context, _ string, t assistant.Turn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = append(m.turns, t)
}

func (m *memHistory) Clear(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = nil
	return nil
}

// TestAnswerAfterErasureForgetsHistory [F24-64] 刪除前送出的問題，刪除後才回答完：問答紀錄清掉、不回答案.
func TestAnswerAfterErasureForgetsHistory(t *testing.T) {
	tests := []struct {
		name       string
		erased     bool
		wantTurns  int
		wantInText string
	}{
		{"no erasure keeps history and answers", false, 1, "雞胸肉沙拉"},
		{"sent before erasure forgets history and says erased", true, 0, erasedMessage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newQAFixture(t, "晚餐可以選雞胸肉沙拉。")
			hist := &memHistory{}
			f.h.WithAssistant(assistant.New(&assistant.Config{Completer: f.ai, History: hist}), nil)
			ctx := context.Background()
			if tt.erased {
				ctx = erasedBeforeNow(t, f.h, "U1")
			}

			f.h.answer(ctx, "U1", "晚餐吃什麼比較好", "tok")

			reply := f.api.lastReply(t)
			if !strings.Contains(reply, tt.wantInText) {
				t.Errorf("reply missing %q: %s", tt.wantInText, reply)
			}
			if tt.erased && strings.Contains(reply, "雞胸肉沙拉") {
				t.Errorf("answer must not be sent after erasure: %s", reply)
			}
			if got := len(hist.Recent(context.Background(), "U1")); got != tt.wantTurns {
				t.Errorf("history turns = %d, want %d", got, tt.wantTurns)
			}
		})
	}
}

// TestClearDeleteConfirmOnReadError [F24-63] 讀不到對話狀態時，非文字訊息仍清掉刪除確認（clearState 被呼叫）.
func TestClearDeleteConfirmOnReadError(t *testing.T) {
	rec := &warnRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := NewWebhookHandler(nil, nil, nil, nil, conversation.NewManager(unreachableRedis(t)))

	h.clearDeleteConfirm(context.Background(), "U1")

	if got := rec.count("clear conversation state"); got != 1 {
		t.Errorf("clear conversation state logged %d times, want 1 (clearState must run when the state cannot be read)", got)
	}
}
