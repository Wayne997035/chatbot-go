package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 開關通知的指令：地震海嘯直接訂閱；天氣特報選完縣市才訂閱；關閉時刪除.
func TestAlertKeywordsSubscribeAndUnsubscribe(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())
	say := func(text string) string {
		t.Helper()
		h.handleTextMessage(context.Background(), "U1", text, "tok")
		return api.lastReply(t)
	}

	mustContain(t, say(keywordQuakeOn), "已開啟地震及海嘯警告通知")
	if sub, ok := alerts.saved["U1_"+alertsub.TypeEarthquakeTsunami]; !ok || !sub.Enabled || sub.UserID != "U1" {
		t.Fatalf("earthquake subscription: %+v", alerts.saved)
	}

	mustContain(t, say(keywordWeatherWarningOn), "台北市", "高雄市")
	mustContain(t, say("臺北市"), "訂閱台北市中正區")
	mustContain(t, say("整個台北市"), "已加入「台北市」")
	mustContain(t, say("火星市"), "找不到此區域")
	mustContain(t, say("訂閱高雄市鳳山區"), "已加入「高雄市鳳山區」")
	if _, ok := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; ok {
		t.Fatal("nothing is saved before the user finishes choosing")
	}
	mustContain(t, say("完成"), "已開啟天氣特報通知", "台北市、高雄市鳳山區")
	if sub := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; !slices.Equal(sub.Regions, []string{"台北市", "高雄市鳳山區"}) || !sub.Enabled {
		t.Fatalf("weather warning subscription: %+v", sub)
	}

	mustContain(t, say(keywordWeatherWarningOff), "已關閉天氣特報通知")
	mustContain(t, say(keywordQuakeOff), "已關閉地震及海嘯警告通知")
	if len(alerts.saved) != 0 {
		t.Errorf("both subscriptions are removed: %+v", alerts.saved)
	}
}

// 選縣市時直接輸入「完成」：不訂閱，也不留下對話狀態.
func TestWeatherWarningWithoutRegionsIsCanceled(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	conv := conversation.NewMemoryManager()
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conv)
	h.handleTextMessage(context.Background(), "U1", keywordWeatherWarningOn, "tok")
	h.handleTextMessage(context.Background(), "U1", "完成", "tok")

	mustContain(t, api.lastReply(t), "已取消天氣特報訂閱設定")
	if state, _ := conv.Get(context.Background(), "U1"); state != nil || len(alerts.saved) != 0 {
		t.Errorf("state=%+v saved=%+v", state, alerts.saved)
	}
}

// 失敗時的 log 只記錯誤，不記 LINE userId（個資）.
func TestFailureLogsDoNotContainTheUserID(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{upsertErr: errors.New("mongo down")}
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h.handleTextMessage(context.Background(), "Uprivate42", keywordQuakeOn, "tok")
	h.handleTextMessage(context.Background(), "Uprivate42", keywordQuakeOff, "tok")

	mustContain(t, buf.String(), "subscribe earthquake tsunami", "mongo down")
	if strings.Contains(buf.String(), "Uprivate42") {
		t.Errorf("the user ID leaked into the log: %s", buf.String())
	}
}

// lastMessage 最後一則回覆的第一個訊息物件.
func lastMessage(t *testing.T, api *lineAPI) map[string]any {
	t.Helper()
	if len(api.replies) == 0 {
		t.Fatal("no reply sent")
	}
	msgs, _ := api.replies[len(api.replies)-1]["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("reply has no messages")
	}
	m, _ := msgs[0].(map[string]any)
	return m
}

// quickReplyTexts 最後一則回覆的快速回覆按鈕送出的文字.
func quickReplyTexts(t *testing.T, api *lineAPI) []string {
	t.Helper()
	qr, _ := lastMessage(t, api)["quickReply"].(map[string]any)
	items, _ := qr["items"].([]any)
	var out []string
	for _, it := range items {
		item, _ := it.(map[string]any)
		action, _ := item["action"].(map[string]any)
		text, _ := action["text"].(string)
		out = append(out, text)
	}
	return out
}

// [F24-98] 點卡片選縣市→鄉鎮：整縣、單一鄉鎮、重複、找不到；每次點選都重設 5 分鐘.
func TestWeatherWarningPickFlow(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())
	say := func(text string) string {
		t.Helper()
		h.handleTextMessage(context.Background(), "U1", text, "tok")
		return api.lastReply(t)
	}

	mustContain(t, say(keywordWeatherWarningOn), "選擇台北市")
	if m := lastMessage(t, api); m["type"] != "flex" || m["altText"] != regionListMessage() {
		t.Errorf("county card: type=%v altText=%v", m["type"], m["altText"])
	}
	mustContain(t, regionListMessage(), "台北市", "高雄市")
	mustContain(t, say("選擇臺北市"), "訂閱台北市中正區")
	mustContain(t, say("台北市"), "訂閱台北市中正區")
	mustContain(t, say("訂閱台北市中正區"), "已加入「台北市中正區」")
	if got := quickReplyTexts(t, api); !slices.Equal(got, []string{"完成訂閱"}) {
		t.Errorf("quick reply after adding: %v", got)
	}
	mustContain(t, say("訂閱整個台北市"), "已加入「台北市」")
	mustContain(t, say("台北市大安區"), "已訂閱整個台北市")
	mustContain(t, say("訂閱整個台北市"), "已在訂閱清單中")
	mustContain(t, say("訂閱高雄市鳳山區"), "已加入「高雄市鳳山區」")
	mustContain(t, say("訂閱高雄市鳳山區"), "已在訂閱清單中")
	mustContain(t, say("訂閱台北市火星區"), "找不到此區域")
	mustContain(t, say("完成訂閱"), "已開啟天氣特報通知")
	if sub := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; !slices.Equal(sub.Regions, []string{"台北市", "高雄市鳳山區"}) {
		t.Errorf("saved regions: %v (中正區 must be replaced by the whole county)", sub.Regions)
	}

	// 選縣市、打錯字都重設 5 分鐘：沒重設的話，最後一步已超過開啟或上一次重設後 5 分鐘.
	// 快取在背景 goroutine 也會讀時鐘，所以用原子變數.
	var nowNano atomic.Int64
	nowNano.Store(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNano.Load()) }
	h = NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManagerWithClock(clock))
	step := func(user, text string, after time.Duration) string {
		t.Helper()
		nowNano.Add(int64(after))
		h.handleTextMessage(context.Background(), user, text, "tok")
		return api.lastReply(t)
	}
	step("U2", keywordWeatherWarningOn, 0)
	step("U2", "選擇台北市", 4*time.Minute)
	mustContain(t, step("U2", "訂閱台北市中正區", 4*time.Minute), "已加入「台北市中正區」")

	step("U3", keywordWeatherWarningOn, 0)
	step("U3", "選擇台北市", 4*time.Minute)
	step("U3", "訂閱台北市火星區", 4*time.Minute)
	mustContain(t, step("U3", "訂閱台北市中正區", 4*time.Minute), "已加入「台北市中正區」")
}

// [F24-98] 對話狀態過期後再點舊卡片：提示重新開始，不誤查天氣、不誤存訂閱.
func TestPickerTapAfterExpiryAsksToRestart(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())
	for _, tap := range []string{"選擇台北市", "訂閱整個台北市", "訂閱台北市中正區", "完成訂閱"} {
		h.handleTextMessage(context.Background(), "U1", tap, "tok")
		mustContain(t, api.lastReply(t), "已逾時")
		if got := quickReplyTexts(t, api); !slices.Equal(got, []string{keywordWeatherWarningOn}) {
			t.Errorf("%s: quick reply %v", tap, got)
		}
	}
	if len(alerts.saved) != 0 {
		t.Errorf("nothing may be saved: %v", alerts.saved)
	}
	h.handleTextMessage(context.Background(), "U1", "台北市", "tok")
	if strings.Contains(api.lastReply(t), "已逾時") {
		t.Error("typing a county name is a weather query, not an expired tap")
	}
}

// [F24-98] 開啟地震海嘯警告時說清楚會收到什麼.
func TestQuakeSubscribeExplainsTrigger(t *testing.T) {
	api := newLineAPI(t, nil)
	alerts := &fakeAlertSubs{subs: map[string][]string{}}
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())
	h.handleTextMessage(context.Background(), "U1", keywordQuakeOn, "tok")
	mustContain(t, api.lastReply(t), "已開啟地震及海嘯警告通知", "顯著有感地震報告", "3 小時")
	mustContain(t, helpMessage, "顯著有感地震報告")
}

// [F30-64] 選地區的過程中打已知指令：照指令執行、選地區狀態清掉（之後的「完成」不會存下選到一半的清單）；
// 打錯字（火星市）仍回「找不到此區域」並保留狀態.
func TestRegionSelectPassesThroughKnownCommands(t *testing.T) {
	cmds := []struct {
		name, text, want string
	}{
		{"help", "說明", "我可以幫你"},
		{"quake off", keywordQuakeOff, "已關閉地震及海嘯警告通知"},
		{"today stats", keywordToday, "今日熱量收支"},
	}
	for _, tc := range cmds {
		t.Run(tc.name, func(t *testing.T) {
			api := newLineAPI(t, nil)
			alerts := &fakeAlertSubs{subs: map[string][]string{}}
			conv := conversation.NewMemoryManager()
			f := &fitnessFixture{api: api, workouts: &storagetest.Workouts{}, profiles: storagetest.NewProfiles(), meals: &storagetest.Meals{}}
			h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conv).
				WithNutrition(nil, f.meals, nil).
				WithFitness(f.workouts, f.profiles, nil)
			say := func(text string) string {
				t.Helper()
				h.handleTextMessage(context.Background(), "U1", text, "tok")
				return api.lastReply(t)
			}

			say(keywordWeatherWarningOn)
			mustContain(t, say("訂閱高雄市鳳山區"), "已加入「高雄市鳳山區」")
			mustContain(t, say(tc.text), tc.want)
			if state, _ := conv.Get(context.Background(), "U1"); state != nil {
				t.Errorf("picker state must be cleared after %q, got %+v", tc.text, state)
			}
			// 之後的「完成」不能把選到一半的清單存下來.
			say("完成")
			if _, ok := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; ok {
				t.Errorf("half-chosen list was saved: %+v", alerts.saved)
			}
		})
	}

	t.Run("typos keep the picker", func(t *testing.T) {
		api := newLineAPI(t, nil)
		alerts := &fakeAlertSubs{subs: map[string][]string{}}
		conv := conversation.NewMemoryManager()
		h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conv)
		h.handleTextMessage(context.Background(), "U1", keywordWeatherWarningOn, "tok")
		for _, typo := range []string{"火星市", "訂閱台北市火星區"} {
			h.handleTextMessage(context.Background(), "U1", typo, "tok")
			mustContain(t, api.lastReply(t), "找不到此區域")
			if state, _ := conv.Get(context.Background(), "U1"); state == nil || state.Type != conversation.TypeWeatherRegionSelect {
				t.Errorf("%q must keep the picker state, got %+v", typo, state)
			}
		}
	})
}
