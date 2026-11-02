package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeUserRepo struct {
	mu    sync.Mutex
	users map[string]bool
}

func newFakeUserRepo() *fakeUserRepo { return &fakeUserRepo{users: map[string]bool{}} }

func (f *fakeUserRepo) Upsert(_ context.Context, u *userstore.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = true
	return nil
}

func (f *fakeUserRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, id)
	return nil
}

// panicGeocoder 模擬依賴裡的 bug（例如以前 Redis 沒連上時的空指標）.
type panicGeocoder struct{}

func (panicGeocoder) Geocode(context.Context, string) ([]geocoding.GeocodeResult, error) {
	panic("nil redis client")
}

type rawEvent struct {
	Type       string            `json:"type"`
	ReplyToken string            `json:"replyToken"`
	Source     map[string]string `json:"source"`
	Message    map[string]string `json:"message,omitempty"`
}

func webhookBody(t *testing.T, events ...rawEvent) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"destination": "Ubot", "events": events})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func textEvent(source map[string]string, text string) rawEvent {
	return rawEvent{Type: "message", ReplyToken: "tok", Source: source, Message: map[string]string{"id": "1", "type": "text", "text": text}}
}

var fromUser = map[string]string{"type": "user", "userId": "U1"}

// 一則事件出錯（panic）不能讓整個服務結束，同一批的其他事件照常處理.
func TestPanicInOneEventDoesNotStopTheBatch(t *testing.T) {
	api := newLineAPI(t, nil)
	h := NewWebhookHandlerWithOptions(api.client, newFakeUserRepo(), weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil,
		conversation.NewMemoryManager(), panicGeocoder{}, nil)

	h.processEvents(context.Background(), webhookBody(t, textEvent(fromUser, "某個小地方"), textEvent(fromUser, "說明")))

	if len(api.replies) != 1 || !strings.Contains(api.lastReply(t), "我可以幫你") {
		t.Errorf("the second event must still be answered: %d replies", len(api.replies))
	}
}

// 群組與聊天室的訊息不處理（否則每則都會送去付費的 AI）；被加進群組時說明一次.
func TestOnlyOneOnOneChatsAreHandled(t *testing.T) {
	api := newLineAPI(t, nil)
	users := newFakeUserRepo()
	h := NewWebhookHandler(api.client, users, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
	group := map[string]string{"type": "group", "groupId": "Cgroup", "userId": "U2"}
	room := map[string]string{"type": "room", "roomId": "Rroom", "userId": "U3"}

	h.processEvents(context.Background(), webhookBody(t,
		rawEvent{Type: "join", ReplyToken: "tok", Source: group},
		textEvent(group, "說明"),
		textEvent(room, "說明"),
		textEvent(map[string]string{"type": "user", "userId": ""}, "說明"),
		textEvent(fromUser, "說明"),
	))

	if len(api.replies) != 2 {
		t.Fatalf("want the join notice and the one-on-one reply, got %d replies", len(api.replies))
	}
	first, _ := json.Marshal(api.replies[0])
	if !strings.Contains(string(first), groupNotice) {
		t.Errorf("join reply: %s", first)
	}
	if users.users["U2"] || users.users["U3"] || users.users[""] || !users.users["U1"] {
		t.Errorf("only the one-on-one user is saved: %v", users.users)
	}
}

type fakeAlertSubs struct {
	subs      map[string][]string // userID → alert types
	saved     map[string]alertsub.AlertSubscription
	upsertErr error
	deleteErr error
}

func (f *fakeAlertSubs) Upsert(_ context.Context, sub *alertsub.AlertSubscription) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if f.saved == nil {
		f.saved = map[string]alertsub.AlertSubscription{}
	}
	f.saved[sub.ID] = *sub
	return nil
}

func (f *fakeAlertSubs) Delete(_ context.Context, id string) error {
	delete(f.saved, id)
	return nil
}

func (f *fakeAlertSubs) DeleteByUser(_ context.Context, userID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.subs, userID)
	return nil
}

type eraseFixture struct {
	*mealFixture
	users  *fakeUserRepo
	alerts *fakeAlertSubs
	works  *storagetest.Workouts
}

func newEraseFixture(t *testing.T, texts ...string) *eraseFixture {
	t.Helper()
	f := &eraseFixture{
		mealFixture: newMealFixture(t, texts...),
		users:       newFakeUserRepo(),
		alerts:      &fakeAlertSubs{subs: map[string][]string{"U1": {"earthquake_tsunami"}, "U2": {"earthquake_tsunami"}}},
		works:       &storagetest.Workouts{},
	}
	f.h.userRepo, f.h.alertSubRepo = f.users, f.alerts
	f.h.WithFitness(f.works, storagetest.NewProfiles(), nil)
	f.users.users["U1"], f.users.users["U2"] = true, true
	return f
}

// 「刪除我的資料」要真的刪掉所有個人資料，包含通知訂閱與使用者紀錄（以前這兩樣會留著、警報照推）.
func TestDeleteMyDataRemovesSubscriptionsAndTheUserRecord(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.Items = append(f.meals.Items, meal.Meal{ID: "other", UserID: "U2", EatenAt: time.Now(), CreateTime: time.Now()})
	_ = f.works.Insert(context.Background(), &workout.Workout{UserID: "U2", Activity: "跑步", DoneAt: time.Now()})

	mustContain(t, f.say(t, keywordDeleteData), "天氣特報與地震通知的訂閱")
	reply := f.say(t, "確認刪除")

	mustContain(t, reply, "已刪除你的所有個人資料")
	if _, ok := f.alerts.subs["U1"]; ok {
		t.Error("alert subscriptions must be deleted")
	}
	if f.users.users["U1"] {
		t.Error("the user record must be deleted")
	}
	if len(f.meals.Items) != 1 || f.meals.Items[0].UserID != "U2" {
		t.Errorf("only U1's meals are deleted: %+v", f.meals.Items)
	}
	if _, ok := f.alerts.subs["U2"]; !ok || !f.users.users["U2"] || len(f.works.Items) != 1 {
		t.Error("other users are untouched")
	}
}

// 某一部分刪除失敗時，其他部分照樣刪，並告訴使用者重試.
func TestDeleteMyDataTriesEveryPart(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.alerts.deleteErr = errors.New("mongo down")
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordDeleteData)

	mustContain(t, f.say(t, "確認刪除"), "部分資料刪除失敗")
	if len(f.meals.Items) != 0 || f.users.users["U1"] {
		t.Errorf("the other parts are still deleted: meals=%d user=%v", len(f.meals.Items), f.users.users["U1"])
	}
}

// 刪除前送出、刪除後才分析完的照片，不能把資料寫回去；刪除之後才傳的訊息照常記錄.
func TestWorkFinishingAfterDeletionIsNotSaved(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	before := time.Now().Add(-time.Second)
	f.say(t, keywordDeleteData)
	f.say(t, "確認刪除")

	f.ai.image = bentoPhoto
	ev := imageEvent()
	f.h.handleMessage(withSentAt(context.Background(), before.UnixMilli()), ev)
	if len(f.meals.Items) != 0 {
		t.Fatalf("a photo sent before the deletion must not be saved: %+v", f.meals.Items)
	}
	mustContain(t, f.api.lastReply(t), erasedMessage)

	f.h.handleTextMessage(withSentAt(context.Background(), time.Now().UnixMilli()), "U1", "午餐吃了牛肉麵", "tok")
	if len(f.meals.Items) != 1 {
		t.Errorf("a message sent after the deletion is saved normally: %+v", f.meals.Items)
	}
}

// 打字記運動也要擋掉不合理的數字（以前只有 AI 解析有上限，「跑步 9999 小時」會照存）.
func TestImplausibleTypedWorkoutIsNotSaved(t *testing.T) {
	f := newFitnessFixture(t, "")
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70}
	for _, text := range []string{"跑步 9999 小時", "跑步 99999 大卡", "騎車 900 公里 2 小時"} {
		if reply := f.say(t, text); !strings.Contains(reply, "看起來不太對") {
			t.Errorf("%s: %s", text, reply)
		}
	}
	if len(f.workouts.Items) != 0 {
		t.Fatalf("nothing implausible may be saved: %+v", f.workouts.Items)
	}
	if reply := f.say(t, "跑步 5公里 30分鐘"); !strings.Contains(reply, "已記錄") || len(f.workouts.Items) != 1 {
		t.Errorf("normal workouts still work: %s", reply)
	}
}

// 加好友時歡迎並說明功能；封鎖時停止通知（推不到也會白白嘗試，佔用額度）.
func TestFollowWelcomesAndUnfollowStopsNotifications(t *testing.T) {
	api := newLineAPI(t, nil)
	users := newFakeUserRepo()
	alerts := &fakeAlertSubs{subs: map[string][]string{"U1": {"earthquake_tsunami"}, "U2": {"weather_warning"}}}
	h := NewWebhookHandler(api.client, users, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), alerts, conversation.NewMemoryManager())

	h.processEvents(context.Background(), webhookBody(t, rawEvent{Type: "follow", ReplyToken: "tok", Source: fromUser}))
	if len(api.replies) != 1 || !strings.Contains(api.lastReply(t), "嗨，我是生活小幫手") || !strings.Contains(api.lastReply(t), "我可以幫你") {
		t.Fatalf("welcome: %d replies", len(api.replies))
	}
	if !users.users["U1"] {
		t.Error("following creates the user record")
	}

	h.processEvents(context.Background(), webhookBody(t, rawEvent{Type: "unfollow", Source: fromUser}))
	if _, ok := alerts.subs["U1"]; ok {
		t.Error("unfollow must stop notifications")
	}
	if _, ok := alerts.subs["U2"]; !ok || len(api.replies) != 1 {
		t.Error("other users are untouched and unfollow sends nothing")
	}
}
