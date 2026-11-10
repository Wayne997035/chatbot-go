package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/weather"
	"context"
	"testing"
)

// erasingSubs [F242-06] 在 fakeAlertSubs 外多記 Upsert 次數，並可在 Upsert 前插入動作（模擬檢查與寫入之間完成刪除）.
type erasingSubs struct {
	*fakeAlertSubs
	upserts      int
	beforeUpsert func()
}

func (e *erasingSubs) Upsert(ctx context.Context, sub *alertsub.AlertSubscription) error {
	e.upserts++
	if e.beforeUpsert != nil {
		e.beforeUpsert()
	}
	return e.fakeAlertSubs.Upsert(ctx, sub)
}

// subscribeFixture 每一列各自新建 handler、erasingSubs 與 line API，不共用（刪除標記與訂閱會互相污染）.
type subscribeFixture struct {
	api  *lineAPI
	subs *erasingSubs
	conv *conversation.Manager
	h    *WebhookHandler
}

func newSubscribeFixture(t *testing.T) *subscribeFixture {
	t.Helper()
	f := &subscribeFixture{
		api:  newLineAPI(t, nil),
		subs: &erasingSubs{fakeAlertSubs: &fakeAlertSubs{subs: map[string][]string{}}},
		conv: conversation.NewMemoryManager(),
	}
	f.h = NewWebhookHandler(f.api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), f.subs, f.conv)
	return f
}

// enterWeatherWarning 天氣特報先用乾淨的 ctx 進入選擇並選好縣市，最後一則「完成」才由 send 帶指定的 ctx 送出.
func (f *subscribeFixture) enterWeatherWarning(t *testing.T) {
	t.Helper()
	f.h.handleTextMessage(t.Context(), "U1", keywordWeatherWarningOn, "tok")
	f.h.handleTextMessage(t.Context(), "U1", "整個台北市", "tok")
}

// send 送出訂閱的最後一則訊息：地震海嘯只有一則；天氣特報是「完成」（之前先呼叫 enterWeatherWarning）.
func (f *subscribeFixture) send(ctx context.Context, weatherWarning bool) {
	if weatherWarning {
		f.h.handleTextMessage(ctx, "U1", "完成", "tok")
		return
	}
	f.h.handleTextMessage(ctx, "U1", keywordQuakeOn, "tok")
}

func (f *subscribeFixture) key(weatherWarning bool) string {
	if weatherWarning {
		return "U1_" + alertsub.TypeWeatherWarning
	}
	return "U1_" + alertsub.TypeEarthquakeTsunami
}

func (f *subscribeFixture) assertNoState(t *testing.T) {
	t.Helper()
	state, err := f.conv.Get(context.Background(), "U1")
	if err != nil || state != nil {
		t.Errorf("conversation state = %+v (err %v), want nil", state, err)
	}
}

// TestSubscribeSentBeforeErasureIsNotSaved [F242-06] 寫入前：刪除資料之前送出的訂閱訊息（含 LINE 重送的舊事件）
// 在刪除之後才處理時，不寫回含 userID 的訂閱文件；沒刪過的人照常訂閱（正對照）.
func TestSubscribeSentBeforeErasureIsNotSaved(t *testing.T) {
	tests := []struct {
		name    string
		weather bool
		erased  bool
	}{
		{"quake_no_erasure", false, false},
		{"quake_sent_before_erasure", false, true},
		{"weather_no_erasure", true, false},
		{"weather_sent_before_erasure", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSubscribeFixture(t)
			if tt.weather {
				f.enterWeatherWarning(t)
			}
			ctx := sentMinuteAgo()
			if tt.erased {
				ctx = erasedBeforeNow(t, f.h, "U1")
			}
			f.send(ctx, tt.weather)

			_, saved := f.subs.saved[f.key(tt.weather)]
			if !tt.erased {
				if f.subs.upserts != 1 || !saved {
					t.Errorf("upserts = %d saved = %v, want 1 and true", f.subs.upserts, saved)
				}
				if tt.weather {
					mustContain(t, f.api.lastReply(t), "已開啟天氣特報通知")
				} else {
					mustContain(t, f.api.lastReply(t), "已開啟地震及海嘯警告通知")
				}
				return
			}
			if f.subs.upserts != 0 || saved {
				t.Errorf("upserts = %d saved = %v, want 0 and false", f.subs.upserts, saved)
			}
			mustContain(t, f.api.lastReply(t), erasedMessage)
			if tt.weather {
				f.assertNoState(t)
			}
		})
	}
}

// TestSubscribeErasedDuringUpsertIsRemoved [F242-06] 寫後再查：檢查與寫入之間完成的刪除，寫完再查一次看得到標記，
// 剛寫的那筆刪掉；沒刪過的人訂閱留下（正對照）.
func TestSubscribeErasedDuringUpsertIsRemoved(t *testing.T) {
	tests := []struct {
		name    string
		weather bool
		erased  bool
	}{
		{"quake_no_erasure", false, false},
		{"quake_erased_during_upsert", false, true},
		{"weather_no_erasure", true, false},
		{"weather_erased_during_upsert", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSubscribeFixture(t)
			if tt.weather {
				f.enterWeatherWarning(t)
			}
			if tt.erased {
				f.subs.beforeUpsert = func() { markErased(t, f.h.erased, t.Context(), "U1") }
			}
			f.send(sentMinuteAgo(), tt.weather)

			_, saved := f.subs.saved[f.key(tt.weather)]
			if !tt.erased {
				if !saved {
					t.Errorf("subscription %q must be saved, saved = %+v", f.key(tt.weather), f.subs.saved)
				}
				return
			}
			if f.subs.upserts != 1 {
				t.Errorf("upserts = %d, want 1 (not erased yet at the pre-write check)", f.subs.upserts)
			}
			if saved {
				t.Errorf("subscription %q must be removed after erasure", f.key(tt.weather))
			}
			mustContain(t, f.api.lastReply(t), erasedMessage)
			if tt.weather {
				f.assertNoState(t)
			}
		})
	}
}
