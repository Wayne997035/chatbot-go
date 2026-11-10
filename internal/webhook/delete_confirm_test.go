package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"context"
	"strings"
	"testing"
)

func nonTextEvent(msgType string) *models.Event {
	return &models.Event{
		Type: "message", ReplyToken: "tok", Source: models.Source{UserID: "U1"},
		Message: models.Message{ID: "9", Type: msgType, ContentProvider: models.ContentProvider{Type: "line"}},
	}
}

// [F24-06] 刪除確認後傳了非文字訊息，之後回「好」不能刪資料.
func TestNonTextMessageClearsDeleteConfirm(t *testing.T) {
	for _, msgType := range []string{"sticker", "image", "location", "video", "audio", "file", "unknown-type"} {
		t.Run(msgType, func(t *testing.T) {
			f := newEraseFixture(t, noodleText)
			f.say(t, "午餐吃了牛肉麵")
			mustContain(t, f.say(t, keywordDeleteData), "確定要刪除")

			f.h.handleMessage(context.Background(), nonTextEvent(msgType))

			state, err := f.h.convManager.Get(context.Background(), "U1")
			if err != nil || (state != nil && state.Type == conversation.TypeDeleteDataConfirm) {
				t.Fatalf("delete confirm must be cleared, state=%+v err=%v", state, err)
			}
			reply := f.say(t, "確認刪除")
			if strings.Contains(reply, "已刪除你的所有個人資料") {
				t.Errorf("must not delete after a non-text message: %s", reply)
			}
			if len(f.meals.Items) != 1 || !f.users.users["U1"] {
				t.Errorf("data must be untouched: meals=%d user=%v", len(f.meals.Items), f.users.users["U1"])
			}
		})
	}
}

// [F24-06] 非文字訊息只清刪除確認，等店家位置的狀態要保留.
func TestNonTextKeepsOtherStates(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordMealPlace)

	ev := nonTextEvent("location")
	ev.Message.Title = "鼎泰豐"
	ev.Message.Address = "台北市信義路二段"
	f.h.handleMessage(context.Background(), ev)

	if p := f.meals.Items[0].Place; p == nil || !strings.Contains(p.Name, "鼎泰豐") {
		t.Fatalf("location must still be taken as the meal place: %+v", p)
	}
}

// [F24-06] 文字「確認刪除」照舊會刪（不被非文字清除邏輯影響）.
func TestTextConfirmStillDeletes(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordDeleteData)
	mustContain(t, f.say(t, "確認刪除"), "已刪除你的所有個人資料")
	if len(f.meals.Items) != 0 {
		t.Errorf("meals must be deleted: %+v", f.meals.Items)
	}
}

// wantStaleDeleteReply 使用者決定的過期提示字面（GTD 決策 735513c0②）；測試寫死字面，不引用被測的常數.
const wantStaleDeleteReply = "刪除確認已經過期，要刪除請再輸入「刪除我的資料」"

// TestStaleDeleteConfirmRepliesExpired [F293-05] 沒有在等刪除確認時收到「確認刪除」：回過期提示，不刪資料、不當成地名查天氣.
func TestStaleDeleteConfirmRepliesExpired(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")

	reply := replyTextOf(t, f.say(t, "確認刪除"))

	if reply != wantStaleDeleteReply {
		t.Errorf("reply = %q, want %q", reply, wantStaleDeleteReply)
	}
	if len(f.meals.Items) != 1 || !f.users.users["U1"] {
		t.Errorf("data must be untouched: meals=%d user=%v", len(f.meals.Items), f.users.users["U1"])
	}
}

// TestDeleteConfirmButtonSendsConfirmDelete [F293-05] 刪除確認的按鈕送出「確認刪除」，不是和運動確認共用的「確認」.
func TestDeleteConfirmButtonSendsConfirmDelete(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	mustContain(t, f.say(t, keywordDeleteData), `"text":"確認刪除"`)
}
