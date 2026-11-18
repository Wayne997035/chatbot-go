package webhook

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/conversation"
	"context"
	"testing"
	"time"
)

// [F34-17] 打字「確認」「取消」（規格 #15-#23）. 沒有待確認時固定回 editNoPendingMessage；只認全等字串.

// seedPendingEdit U1 有一筆麵包 120 大卡與一張「改成 210」的待確認卡，回傳確認鈕 data.
func seedPendingEdit(t *testing.T, f *editFixture) string {
	t.Helper()
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	return f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
}

// 規格 #15：U2 打「確認」「取消」→ 沒有待確認；U1 的卡沒被讀走也沒被消耗，資料不變，之後 U1 仍可確認.
func TestTextConfirmOtherUserNoPending(t *testing.T) {
	f := newEditFixture(t)
	ok := seedPendingEdit(t, f)
	mustContain(t, f.sayAs(t, "U2", "確認"), editNoPendingMessage)
	f.quiet("U2", "取消")
	mustContain(t, f.api.lastReply(t), editNoPendingMessage)
	p, err := f.store.PeekPending(context.Background(), "U1", time.Now())
	if err != nil || p == nil || p.Kind != recordedit.OutcomeProposal {
		t.Fatalf("U1's pending after U2 typed confirm/cancel: %+v %v", p, err)
	}
	if f.mealTotal() != 120 || f.meals.Updates != 0 {
		t.Errorf("U1's data changed: total=%v updates=%d", f.mealTotal(), f.meals.Updates)
	}
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
}

// 規格 #16：U2 帶 U1 的 nonce 按確認 → editExpiredMessage；U1 的提議仍在、可再確認成功.
func TestPostbackOtherUserNonceKeepsPending(t *testing.T) {
	f := newEditFixture(t)
	ok := seedPendingEdit(t, f)
	mustContain(t, f.tapReply(t, "U2", ok), editExpiredMessage)
	if f.store.Peek("U1") == nil {
		t.Fatal("U2's tap with U1's nonce consumed U1's pending")
	}
	if f.mealTotal() != 120 || f.meals.Updates != 0 {
		t.Errorf("U1's data changed: total=%v updates=%d", f.mealTotal(), f.meals.Updates)
	}
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
}

// 規格 #18：出卡後 5 分 1 秒打「確認」→ 沒有待確認、零寫入；4 分 59 秒內打則成功（邊界兩側）.
func TestTextConfirmAfterExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		after     time.Duration
		wantReply string
		wantTotal float64
	}{
		{"5m01s: expired", recordedit.PendingTTL + time.Second, editNoPendingMessage, 120},
		{"exactly 5m: expired", recordedit.PendingTTL, editNoPendingMessage, 120},
		{"4m59s: still valid", recordedit.PendingTTL - time.Second, "已改成 210", 210},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditFixture(t)
			base := time.Now()
			f.setNow(base)
			seedPendingEdit(t, f)
			f.setNow(base.Add(tc.after))
			mustContain(t, f.sayAs(t, "U1", "確認"), tc.wantReply)
			if f.mealTotal() != tc.wantTotal {
				t.Errorf("total = %v, want %v", f.mealTotal(), tc.wantTotal)
			}
			if tc.wantTotal == 120 && f.meals.Updates != 0 {
				t.Error("an expired confirm wrote")
			}
		})
	}
}

// 規格 #19：打「取消」→ 好，沒有修改；暫存清空、零寫入；再打「確認」回沒有待確認.
func TestTextCancelPending(t *testing.T) {
	f := newEditFixture(t)
	seedPendingEdit(t, f)
	mustContain(t, f.sayAs(t, "U1", "取消"), editCancelledMessage)
	if f.store.Len() != 0 {
		t.Error("the pending survived a typed cancel")
	}
	f.assertNothingWritten(t, 1, 0)
	if f.mealTotal() != 120 {
		t.Errorf("total = %v", f.mealTotal())
	}
	mustContain(t, f.sayAs(t, "U1", "確認"), editNoPendingMessage)
}

// 規格 #20：功能啟用、沒有待確認，打「確認」「取消」各一次 → 兩次都回固定句；問答與規劃器呼叫數皆 0.
func TestTextConfirmNoPending(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	for _, word := range []string{"確認", "取消"} {
		mustContain(t, f.sayAs(t, "U1", word), editNoPendingMessage)
	}
	if n := len(f.qa.asked); n != 0 {
		t.Errorf("the Q&A assistant was called %d times", n)
	}
	if f.planner.calls.Load() != 0 || f.plan.Calls() != 0 {
		t.Errorf("planner calls = %d, model calls = %d, want 0", f.planner.calls.Load(), f.plan.Calls())
	}
	f.assertNothingWritten(t, 1, 0)
}

// 規格 #21：暫存是候選清單時打「確認」→ 請先點選要改哪一筆；候選仍在（不 Take），打「取消」才清掉.
func TestTextConfirmOnClarifyKeepsPending(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-2*time.Hour))
	f.seedMeal(t, "U1", "麵包", 300, time.Now().Add(-time.Minute))
	f.plan.answers = []string{`{"op":"set_item_kcal","item":1,"kcal":150,"candidates":["M1","M2"]}`}
	f.sayAs(t, "U1", "麵包熱量改成 150")
	if p := f.store.Peek("U1"); p == nil || p.Kind != recordedit.OutcomeClarify {
		t.Fatalf("setup: want a candidate pending, got %+v", p)
	}
	takes := f.store.Takes.Load()
	mustContain(t, f.sayAs(t, "U1", "確認"), editPickFirstMessage)
	if p := f.store.Peek("U1"); p == nil || p.Kind != recordedit.OutcomeClarify {
		t.Fatalf("the candidate pending was consumed: %+v", p)
	}
	if f.store.Takes.Load() != takes {
		t.Error("typed confirm on a candidate list called Take")
	}
	if f.meals.Updates != 0 {
		t.Error("typed confirm on a candidate list wrote")
	}
	mustContain(t, f.sayAs(t, "U1", "取消"), editCancelledMessage)
	if f.store.Len() != 0 {
		t.Error("typed cancel did not clear the candidate pending")
	}
}

// 規格 #22：對話狀態優先、不被本步搶走：TypeWorkoutConfirm 的「確認」仍記運動；「確認刪除」維持過期句；「好」「確認！」不進本步.
func TestTextConfirmDoesNotStealWorkoutConfirm(t *testing.T) {
	setWorkoutConfirm := func(t *testing.T, f *editFixture) {
		t.Helper()
		st := &conversation.State{Type: conversation.TypeWorkoutConfirm, Workout: &conversation.PendingWorkout{Activity: "跑步", DurationMin: 30, Calories: 300}}
		if err := f.h.convManager.Set(context.Background(), "U1", st); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("confirm during a workout confirmation still records the workout", func(t *testing.T) {
		f := newEditFixture(t)
		setWorkoutConfirm(t, f)
		f.sayAs(t, "U1", "確認")
		if len(f.workouts.Items) != 1 {
			t.Fatalf("workouts = %d, the confirm must record the workout", len(f.workouts.Items))
		}
		if f.store.Peeks.Load() != 0 {
			t.Error("the typed-confirm step ran before the conversation step")
		}
	})
	t.Run("a pending edit card does not take the workout's confirm", func(t *testing.T) {
		f := newEditFixture(t)
		seedPendingEdit(t, f)
		peeks := f.store.Peeks.Load()
		setWorkoutConfirm(t, f)
		f.sayAs(t, "U1", "確認")
		if len(f.workouts.Items) != 1 {
			t.Errorf("workouts = %d, want the workout recorded", len(f.workouts.Items))
		}
		if f.store.Peek("U1") == nil || f.meals.Updates != 0 || f.store.Peeks.Load() != peeks {
			t.Error("the workout's confirm touched the meal edit card")
		}
	})
	t.Run("確認刪除 keeps the stale delete reply and never reaches the step", func(t *testing.T) {
		f := newEditFixture(t)
		seedPendingEdit(t, f)
		peeks := f.store.Peeks.Load()
		mustContain(t, f.sayAs(t, "U1", keywordConfirmDelete), staleDeleteConfirmMessage)
		if f.store.Peeks.Load() != peeks || f.store.Peek("U1") == nil {
			t.Error("確認刪除 reached the typed-confirm step or consumed the card")
		}
	})
	t.Run("好 and 確認！ do not enter the step", func(t *testing.T) {
		f := newEditFixture(t)
		seedPendingEdit(t, f)
		peeks := f.store.Peeks.Load()
		for _, w := range []string{"好", "確認！"} {
			f.quiet("U1", w)
		}
		if f.store.Peeks.Load() != peeks || f.mealTotal() != 120 {
			t.Errorf("peeks +%d total=%v: the words must not be treated as a confirm", f.store.Peeks.Load()-peeks, f.mealTotal())
		}
	})
}

// 規格 #23：刪除資料標記晚於這則訊息的送出時間 → 回 erasedMessage、零寫入.
func TestTextConfirmAfterErasure(t *testing.T) {
	f := newEditFixture(t)
	seedPendingEdit(t, f)
	if err := f.h.erased.mark(context.Background(), "U1"); err != nil {
		t.Fatal(err)
	}
	ctx := withSentAt(context.Background(), time.Now().Add(-time.Minute).UnixMilli()) // 訊息在刪除之前送出
	f.h.handleTextMessage(ctx, "U1", "確認", "tok")
	mustContain(t, f.api.lastReply(t), erasedMessage)
	if f.mealTotal() != 120 || f.meals.Updates != 0 {
		t.Errorf("written after the erasure mark: total=%v updates=%d", f.mealTotal(), f.meals.Updates)
	}
}

// 規格 #26 / F34-18：EditGate 補詞；E-A 截圖原文（記三餐）不能因新詞表誤進規劃器.
func TestEditGateRegroupVerbs(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"把三明治移到早餐", true},
		{"幫我分成早午晚三餐", true},
		{"那你先幫我分開處理紀錄更新好", true},
		{"把牛肉火腿起司三明治改為早餐", true},
		{"把雞胸肉搬到午餐", true},
		{"把這餐拆成兩筆", true},
		{"早餐吃了三明治", false},
		{"今天吃什麼", false},
		{"午餐推薦", false},
		// E-A 截圖原文：一則訊息記三餐，是記餐不是修改.
		{"早餐吃 牛肉火腿起司三明治\n\n午餐 黑麥酸種麵包兩個手掌大小\n\n晚餐 福隆雞腿便當 加上哈根達斯草莓口味4匙 加上一杯牛奶", false},
	}
	for _, tt := range tests {
		if got := EditGate(tt.text); got != tt.want {
			t.Errorf("EditGate(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
