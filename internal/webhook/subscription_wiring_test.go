package webhook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/subscription"
	"chatbot-go/internal/weather"
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

// ---- F30-04 / F30-08：EraseAll（聊天「確認刪除」與 LIFF 刪除帳號共用）----

// failMeals 讓 DeleteAllByUser 回指定錯誤；其餘照 storagetest.Meals.
type failMeals struct {
	meal.MealRepository
	err    error
	onCall func()
}

func (f failMeals) DeleteAllByUser(ctx context.Context, userID string) error {
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return f.err
	}
	return f.MealRepository.DeleteAllByUser(ctx, userID)
}

// A36：順序固定 ① 清對話狀態 → ② mark → ③ 其餘項目；刪完對話狀態為 nil（含刪除確認與選地區）.
func TestEraseAllOrderIsStateThenMarkThenData(t *testing.T) {
	for _, st := range []conversation.StateType{conversation.TypeDeleteDataConfirm, conversation.TypeWeatherRegionSelect} {
		t.Run(string(st), func(t *testing.T) {
			f := newEraseFixture(t, noodleText)
			ctx := context.Background()
			if err := f.h.convManager.Set(ctx, "U1", &conversation.State{Type: st}); err != nil {
				t.Fatal(err)
			}
			var stateAtMark, stateAtDataDelete *conversation.State
			var markAtDataDelete, sawMark bool
			f.h.erased.now = func() time.Time {
				if !sawMark { // mark 的第一個動作就是讀時鐘
					sawMark = true
					stateAtMark, _ = f.h.convManager.Get(ctx, "U1")
				}
				return time.Now()
			}
			f.h.mealRepo = failMeals{MealRepository: f.meals, onCall: func() {
				stateAtDataDelete, _ = f.h.convManager.Get(ctx, "U1")
				markAtDataDelete = sawMark
			}}

			if err := f.h.EraseAll(ctx, "U1"); err != nil {
				t.Fatalf("EraseAll: %v", err)
			}
			if !sawMark || stateAtMark != nil {
				t.Errorf("conversation state must be cleared before the mark (sawMark=%v state=%+v)", sawMark, stateAtMark)
			}
			if !markAtDataDelete || stateAtDataDelete != nil {
				t.Errorf("mark must be written before data deletion (mark=%v state=%+v)", markAtDataDelete, stateAtDataDelete)
			}
			if s, _ := f.h.convManager.Get(ctx, "U1"); s != nil {
				t.Errorf("state after EraseAll = %+v, want nil", s)
			}
			if f.users.users["U1"] {
				t.Error("user document must be deleted")
			}
			if _, ok := f.alerts.subs["U1"]; ok {
				t.Error("subscriptions must be deleted")
			}
		})
	}
}

// A36 / A11：多項失敗合併回傳、只含項目名（不含 userID 與底層訊息）、其餘項目仍被嘗試；mark 失敗也回錯.
func TestEraseAllJoinsFailuresByItemNameAndKeepsGoing(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.alerts.deleteErr = errors.New("alert store down for U1 a@b.test")
	f.h.mealRepo = failMeals{MealRepository: f.meals, err: errors.New("meals store down for U1 10.0.0.5:27017")}

	err := f.h.EraseAll(context.Background(), "U1")
	if err == nil {
		t.Fatal("EraseAll must fail when parts fail")
	}
	msg := err.Error()
	for _, want := range []string{"erase meals", "erase alert subscriptions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing item %q", msg, want)
		}
	}
	for _, leak := range []string{"U1", "a@b.test", "10.0.0.5", "store down"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks %q", msg, leak)
		}
	}
	if f.users.users["U1"] {
		t.Error("the user document must still be attempted after earlier failures")
	}
}

func TestEraseAllReportsMarkFailureAndStillErases(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	fake := newCtxRedis()
	fake.onSet = func(_ context.Context, key string) error {
		if strings.HasPrefix(key, erasureKeyPrefix) {
			return errors.New("dial tcp 10.0.0.1:6379: i/o timeout")
		}
		return nil
	}
	f.h.erased = newErasures(fake.client(t))
	err := f.h.EraseAll(context.Background(), "U1")
	if err == nil || !strings.Contains(err.Error(), "record erasure mark") {
		t.Fatalf("err = %v, want the mark failure by item name", err)
	}
	if f.users.users["U1"] {
		t.Error("data deletion must still run when the mark fails")
	}
}

// 聊天「確認刪除」與 LIFF 走同一個 EraseAll：確認之後標記已寫、資料已刪.
func TestChatDeleteConfirmGoesThroughEraseAll(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordDeleteData)
	mustContain(t, f.say(t, "確認刪除"), "已刪除你的所有個人資料")
	if !f.h.ErasedSince(context.Background(), "U1", time.Time{}) {
		t.Error("EraseAll must have written the erasure mark")
	}
	if f.users.users["U1"] || len(f.meals.Items) != 0 {
		t.Error("data must be gone")
	}
}

// A21：eraseUserData / clearState 的 log 不含 userID、email、主機:埠.
func TestEraseLogsAreRedacted(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	uid := "U" + strings.Repeat("ab", 16)
	buf := captureSlog(t)
	f.h.mealRepo = failMeals{MealRepository: f.meals, err: errors.New("meals down " + uid + " a@b.test 10.0.0.5:27017")}
	if err := f.h.EraseAll(context.Background(), uid); err == nil {
		t.Fatal("want error")
	}
	if recs := recordsByMsg(t, buf, "delete user data"); len(recs) == 0 {
		t.Fatalf("no delete user data log: %s", buf.String())
	}
	for _, leak := range []string{uid, "a@b.test", "10.0.0.5:27017"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log leaks %q: %s", leak, buf.String())
		}
	}
}
