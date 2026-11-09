package webhook

import (
	"context"
	"errors"
	"testing"
)

// fakeAlertHistory [F241-11] 記錄 ForgetRecipient 收到的 userID，以及收到當下警報訂閱是否已刪.
type fakeAlertHistory struct {
	calls          []string
	subsGoneAtCall []bool
	subs           map[string][]string
	err            error
}

func (f *fakeAlertHistory) ForgetRecipient(_ context.Context, userID string) error {
	f.calls = append(f.calls, userID)
	_, still := f.subs[userID]
	f.subsGoneAtCall = append(f.subsGoneAtCall, !still)
	return f.err
}

// [F241-11] 使用者確認刪除：警報推播紀錄清除者只收到一次他的 userID，且收到時訂閱已刪.
func TestDeleteMyDataForgetsAlertHistory(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	hist := &fakeAlertHistory{subs: f.alerts.subs}
	f.h.WithAlertHistory(hist)

	f.say(t, keywordDeleteData)
	reply := f.say(t, "確認刪除")

	mustContain(t, reply, "已刪除你的所有個人資料")
	if len(hist.calls) != 1 || hist.calls[0] != "U1" {
		t.Fatalf("ForgetRecipient calls = %v, want exactly [U1]", hist.calls)
	}
	if !hist.subsGoneAtCall[0] {
		t.Error("alert subscriptions must already be deleted when alert history is cleared")
	}
}

// [F241-11] 清除失敗要告訴使用者部分失敗，其他部分照刪.
func TestDeleteMyDataReportsAlertHistoryFailure(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	hist := &fakeAlertHistory{subs: f.alerts.subs, err: errors.New("redis down")}
	f.h.WithAlertHistory(hist)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordDeleteData)

	mustContain(t, f.say(t, "確認刪除"), "部分資料刪除失敗")
	if len(f.meals.Items) != 0 || f.users.users["U1"] {
		t.Errorf("the other parts are still deleted: meals=%d user=%v", len(f.meals.Items), f.users.users["U1"])
	}
	if len(hist.calls) != 1 {
		t.Errorf("ForgetRecipient calls = %v, want 1", hist.calls)
	}
}

// [F241-12] 刪除標記 10 分鐘內：ErasedRecently 為 true；沒刪過的人為 false.
func TestWebhookErasedRecently(t *testing.T) {
	f := newEraseFixture(t, noodleText)
	ctx := context.Background()
	if f.h.ErasedRecently(ctx, "U1") {
		t.Fatal("control: U1 has not erased yet")
	}
	f.say(t, keywordDeleteData)
	f.say(t, "確認刪除")
	if !f.h.ErasedRecently(ctx, "U1") {
		t.Error("U1 erased just now, must be true")
	}
	if f.h.ErasedRecently(ctx, "U2") {
		t.Error("U2 never erased, must be false")
	}
}
