package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
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
	mustContain(t, say("臺北市"), "已加入「台北市」")
	mustContain(t, say("台北市"), "已在訂閱清單中")
	mustContain(t, say("火星市"), "找不到此區域")
	mustContain(t, say("高雄市"), "已加入「高雄市」")
	if _, ok := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; ok {
		t.Fatal("nothing is saved before the user finishes choosing")
	}
	mustContain(t, say("完成"), "已開啟天氣特報通知", "台北市、高雄市")
	if sub := alerts.saved["U1_"+alertsub.TypeWeatherWarning]; !slices.Equal(sub.Regions, []string{"台北市", "高雄市"}) || !sub.Enabled {
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
