package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/subscription"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// F30-02 聊天端改接共用訂閱服務的測試.

type fakeSubService struct {
	quakeCalls   int
	weatherCalls [][]string
	disabled     []string
	err          error
	since        []time.Time
}

func (f *fakeSubService) List(context.Context, string) ([]subscription.Item, error) { return nil, nil }

func (f *fakeSubService) EnableQuakeTsunami(_ context.Context, _ string, since time.Time) error {
	f.quakeCalls++
	f.since = append(f.since, since)
	return f.err
}

func (f *fakeSubService) SetWeatherWarningRegions(_ context.Context, _ string, regions []string, since time.Time) error {
	f.weatherCalls = append(f.weatherCalls, slices.Clone(regions))
	f.since = append(f.since, since)
	return f.err
}

func (f *fakeSubService) Disable(_ context.Context, _, alertType string) error {
	f.disabled = append(f.disabled, alertType)
	return f.err
}

func newWiringHandler(t *testing.T, alerts SubscriptionStore) (*WebhookHandler, *lineAPI) {
	t.Helper()
	api := newLineAPI(t, nil)
	return NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager()), api
}

// 沒呼叫 WithSubscription：聊天經預設服務（alertSubRepo 建的），既有建構碼不用改；兩種訂閱都寫進同一個 store.
func TestChatUsesDefaultSubscriptionServiceWhenNotInjected(t *testing.T) {
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h, api := newWiringHandler(t, alerts)
	ctx := context.Background()

	h.handleTextMessage(ctx, "U1", keywordQuakeOn, "tok")
	mustContain(t, api.lastReply(t), "已開啟地震及海嘯警告通知")
	if _, ok := alerts.saved["U1_"+alertsub.TypeEarthquakeTsunami]; !ok {
		t.Fatal("quake subscription not written through default service")
	}
	h.handleTextMessage(ctx, "U1", keywordQuakeOff, "tok")
	mustContain(t, api.lastReply(t), "已關閉地震及海嘯警告通知")
	if _, ok := alerts.saved["U1_"+alertsub.TypeEarthquakeTsunami]; ok {
		t.Fatal("quake subscription still present after disable")
	}
}

// 注入服務：聊天的開、選地區完成、關都只經它，since 帶 LINE 事件時間.
func TestChatRoutesThroughInjectedSubscriptionService(t *testing.T) {
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h, api := newWiringHandler(t, alerts)
	svc := &fakeSubService{}
	h.WithSubscription(svc)
	sent := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := withSentAt(context.Background(), sent.UnixMilli())

	h.handleTextMessage(ctx, "U1", keywordQuakeOn, "tok")
	h.handleTextMessage(ctx, "U1", keywordWeatherWarningOn, "tok")
	h.handleTextMessage(ctx, "U1", "訂閱新北市板橋區", "tok")
	h.handleTextMessage(ctx, "U1", "訂閱整個台北市", "tok")
	h.handleTextMessage(ctx, "U1", "完成訂閱", "tok")
	mustContain(t, api.lastReply(t), "已開啟天氣特報通知")
	h.handleTextMessage(ctx, "U1", keywordWeatherWarningOff, "tok")
	mustContain(t, api.lastReply(t), "已關閉天氣特報通知")

	if svc.quakeCalls != 1 || len(svc.weatherCalls) != 1 || !slices.Equal(svc.disabled, []string{alertsub.TypeWeatherWarning}) {
		t.Fatalf("service calls: quake=%d weather=%v disabled=%v", svc.quakeCalls, svc.weatherCalls, svc.disabled)
	}
	for _, s := range svc.since {
		if !s.Equal(sent) {
			t.Fatalf("since = %v, want the LINE event time %v", s, sent)
		}
	}
	if len(alerts.saved) != 0 {
		t.Fatalf("chat wrote the store directly, bypassing the service: %v", alerts.saved)
	}
}

// 服務回錯誤時的聊天回覆（文案與改動前相同）；ErrErased 與 ErrEraseIncomplete 的天氣特報要一併補刪對話狀態.
func TestChatMapsSubscriptionErrorsToReplies(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"erased", subscription.ErrErased, erasedMessage, eraseFailedMessage},
		{"undo incomplete", subscription.ErrEraseIncomplete, eraseFailedMessage, erasedMessage},
		{"plain error", errors.New("boom"), "失敗，請稍後再試", erasedMessage},
	}
	for _, tc := range cases {
		t.Run("quake/"+tc.name, func(t *testing.T) {
			h, api := newWiringHandler(t, &fakeAlertSubs{subs: map[string][]string{}})
			h.WithSubscription(&fakeSubService{err: tc.err})
			h.handleTextMessage(context.Background(), "U1", keywordQuakeOn, "tok")
			reply := api.lastReply(t)
			mustContain(t, reply, tc.want)
			if strings.Contains(reply, tc.notWant) {
				t.Fatalf("reply %q must not contain %q", reply, tc.notWant)
			}
		})
		t.Run("weather/"+tc.name, func(t *testing.T) {
			h, api := newWiringHandler(t, &fakeAlertSubs{subs: map[string][]string{}})
			h.WithSubscription(&fakeSubService{err: tc.err})
			ctx := context.Background()
			h.handleTextMessage(ctx, "U1", keywordWeatherWarningOn, "tok")
			h.handleTextMessage(ctx, "U1", "訂閱整個台北市", "tok")
			h.handleTextMessage(ctx, "U1", "完成訂閱", "tok")
			reply := api.lastReply(t)
			mustContain(t, reply, tc.want)
			if strings.Contains(reply, tc.notWant) {
				t.Fatalf("reply %q must not contain %q", reply, tc.notWant)
			}
			state, err := h.convManager.GetStrict(ctx, "U1")
			isErased := errors.Is(tc.err, subscription.ErrErased) || errors.Is(tc.err, subscription.ErrEraseIncomplete)
			if err != nil || (isErased && state != nil) {
				t.Fatalf("conversation state must be cleared on erasure (state=%v err=%v)", state, err)
			}
		})
	}
}

// A38：聊天依序點「新北市板橋區」→「台北市」→ 完成，存的是 region.Counties 順序，不是點選順序.
func TestChatStoresRegionsInCountyOrderNotPickOrder(t *testing.T) {
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h, _ := newWiringHandler(t, alerts)
	ctx := context.Background()
	for _, text := range []string{keywordWeatherWarningOn, "訂閱新北市板橋區", "訂閱整個台北市", "完成訂閱"} {
		h.handleTextMessage(ctx, "U1", text, "tok")
	}
	got := alerts.saved["U1_"+alertsub.TypeWeatherWarning].Regions
	if !slices.Equal(got, []string{"台北市", "新北市板橋區"}) {
		t.Fatalf("stored regions %v, want region.Counties order", got)
	}
}

// 聊天補刪沿用 webhook 的補刪 ctx：預設服務的刪除標記查詢就是 handler 自己，所以 UndoContext 被用上.
func TestDefaultServiceUndoUsesHandlerUndoContext(t *testing.T) {
	var _ subscription.UndoContexter = (*WebhookHandler)(nil)
	h, _ := newWiringHandler(t, &fakeAlertSubs{subs: map[string][]string{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	uctx, ucancel := h.UndoContext(ctx)
	defer ucancel()
	if uctx.Err() != nil {
		t.Fatalf("undo ctx follows the cancelled request: %v", uctx.Err())
	}
}

// A29/A34：webhook 的非測試碼不再直接 alertSubRepo.Upsert / .Delete（eraseUserData 的 DeleteByUser 除外）.
func TestWebhookDoesNotWriteSubscriptionsDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"alertSubRepo.Upsert", "alertSubRepo.Delete(", "alertSubRepo.FindByUserID"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s still calls %s directly; subscriptions go through subscription.Service", f, banned)
			}
		}
	}
}
