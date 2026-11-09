package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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

// hookRedis 用 go-redis Hook 做的假 Redis（SET、GET、DEL）；不連線.
// beforeSet 在每次 SET 寫入前呼叫（模擬「檢查與寫入之間」發生的事）；未處理的指令回 error.
type hookRedis struct {
	mu        sync.Mutex
	data      map[string]string
	beforeSet func(key string)
	setCount  map[string]int // 依 key 前綴（冒號之前含冒號）計 SET 次數
}

func newHookRedis() *hookRedis {
	return &hookRedis{data: map[string]string{}, setCount: map[string]int{}}
}

// hookArgStr SET 值的型別以實測為準：string 直接用、[]byte 轉字串、其他 fmt.Sprint.
func hookArgStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func (f *hookRedis) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

func (f *hookRedis) sets(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCount[prefix]
}

func (f *hookRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial blocked") }
}

func (f *hookRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *hookRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		name := strings.ToLower(hookArgStr(args[0]))
		switch c := cmd.(type) {
		case *redis.StatusCmd:
			if name == "set" {
				key := hookArgStr(args[1])
				if f.beforeSet != nil {
					f.beforeSet(key)
				}
				f.mu.Lock()
				f.data[key] = hookArgStr(args[2])
				if i := strings.Index(key, ":"); i >= 0 {
					f.setCount[key[:i+1]]++
				}
				f.mu.Unlock()
				c.SetVal("OK")
				return nil
			}
		case *redis.StringCmd:
			if name == "get" {
				f.mu.Lock()
				v, ok := f.data[hookArgStr(args[1])]
				f.mu.Unlock()
				if !ok {
					// go-redis 用 Hook 的回傳值覆寫 cmd 的錯誤，缺鍵必須同時回傳 redis.Nil.
					c.SetErr(redis.Nil)
					return redis.Nil
				}
				c.SetVal(v)
				return nil
			}
		case *redis.IntCmd:
			if name == "del" {
				f.mu.Lock()
				n := 0
				for _, a := range args[1:] {
					k := hookArgStr(a)
					if _, ok := f.data[k]; ok {
						delete(f.data, k)
						n++
					}
				}
				f.mu.Unlock()
				c.SetVal(int64(n))
				return nil
			}
		}
		err := fmt.Errorf("hookRedis: unsupported %s %T", name, cmd)
		cmd.SetErr(err)
		return err
	}
}

func (f *hookRedis) client(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	c.AddHook(f)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sentMinuteAgo 訊息 1 分鐘前送出的 ctx；刪除發生在 Hook 內，不可用 erasedBeforeNow.
func sentMinuteAgo() context.Context {
	return withSentAt(context.Background(), time.Now().Add(-time.Minute).UnixMilli())
}

// TestForecastDoesNotRememberLocationAfterErasureRace [F241-07] 檢查刪除標記與寫入地點之間剛好完成刪除：地點不留.
func TestForecastDoesNotRememberLocationAfterErasureRace(t *testing.T) {
	forecast := &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區",
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "80"}},
	}
	tests := []struct {
		name         string
		eraseOnSet   bool
		wantRemember bool
	}{
		{"no erasure keeps location", false, true},
		{"erased between check and write drops location", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newHookRedis()
			api := newLineAPI(t, nil)
			h := newWeatherHandler(api.client, "not json")
			h.locations = weather.NewUserLocations(fake.client(t), weather.NewLookup(&fakeWeatherRepo{forecast: forecast}, nil, 60))
			if tt.eraseOnSet {
				fake.beforeSet = func(key string) {
					if strings.HasPrefix(key, "loc2:") {
						h.erased.mark(context.Background(), "U1")
					}
				}
			}

			h.replyForecast(sentMinuteAgo(), "U1", "tok", forecast, "")

			if reply := api.lastReply(t); !strings.Contains(reply, "信義區") {
				t.Errorf("forecast must still be replied: %s", reply)
			}
			if got := fake.has("loc2:U1"); got != tt.wantRemember {
				t.Errorf("loc2:U1 in redis = %v, want %v", got, tt.wantRemember)
			}
			if got := h.locations.ForUser(context.Background(), "U1") != ""; got != tt.wantRemember {
				t.Errorf("ForUser non-empty = %v, want %v", got, tt.wantRemember)
			}
		})
	}
}

// vagueMealHandler 對話狀態用假 Redis 的餐點測試 fixture.
func vagueMealHandler(t *testing.T, fake *hookRedis) *mealFixture {
	t.Helper()
	f := newMealFixture(t, bentoClarify)
	f.h.convManager = conversation.NewManager(fake.client(t))
	return f
}

// TestMealClarifyIsDroppedAfterErasure [F241-10] 刪除後才處理到的籠統餐點：不寫對話狀態（前置檢查擋下）.
func TestMealClarifyIsDroppedAfterErasure(t *testing.T) {
	fake := newHookRedis()
	f := vagueMealHandler(t, fake)
	ctx := sentMinuteAgo()
	f.h.erased.mark(context.Background(), "U1")

	f.h.handleTextMessage(ctx, "U1", "晚餐吃便當", "tok")

	if reply := f.api.lastReply(t); !strings.Contains(reply, erasedMessage) {
		t.Errorf("reply = %s, want erasedMessage", reply)
	}
	if fake.has("conv:U1") {
		t.Error("conv:U1 must not exist in redis")
	}
	if n := fake.sets("conv:"); n != 0 {
		t.Errorf("SET conv:* count = %d, want 0 (the pre-check must stop the write)", n)
	}
}

// TestMealClarifyStateClearedWhenErasedDuringSet [F241-10] 寫對話狀態的當下剛好完成刪除：寫後再查，清掉並回已刪除.
func TestMealClarifyStateClearedWhenErasedDuringSet(t *testing.T) {
	fake := newHookRedis()
	f := vagueMealHandler(t, fake)
	fake.beforeSet = func(key string) {
		if strings.HasPrefix(key, "conv:") {
			f.h.erased.mark(context.Background(), "U1")
		}
	}

	f.h.handleTextMessage(sentMinuteAgo(), "U1", "晚餐吃便當", "tok")

	if reply := f.api.lastReply(t); !strings.Contains(reply, erasedMessage) {
		t.Errorf("reply = %s, want erasedMessage", reply)
	}
	if fake.has("conv:U1") {
		t.Error("conv:U1 must be cleared after the write-back check")
	}
	if st, _ := f.h.convManager.Get(context.Background(), "U1"); st != nil {
		t.Errorf("conversation state = %+v, want nil", st)
	}
}

// TestEraseUserDataClearsConversationState [F241-10] eraseUserData 在 mark 之後清掉 clearState 與 mark 之間寫入的對話狀態.
func TestEraseUserDataClearsConversationState(t *testing.T) {
	fake := newHookRedis()
	h := NewWebhookHandler(nil, nil, nil, nil, conversation.NewManager(fake.client(t)))
	ctx := context.Background()
	state := &conversation.State{Type: conversation.TypeMealClarify, Meal: &conversation.PendingMeal{Text: "晚餐吃便當"}}
	if err := h.convManager.Set(ctx, "U1", state); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if !fake.has("conv:U1") {
		t.Fatal("seed: conv:U1 must exist before erasing")
	}

	failed := h.eraseUserData(ctx, "U1")

	for _, part := range failed {
		if part == "conversation" {
			t.Errorf("conversation must be erased, failed = %v", failed)
		}
	}
	if fake.has("conv:U1") {
		t.Error("conv:U1 must be gone after eraseUserData")
	}
}
