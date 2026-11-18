package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingMeals 讓 Update / DeleteByID 回錯誤的餐點儲存（DB 錯誤注入；其餘行為交給真的記憶體實作）.
type failingMeals struct {
	meal.MealRepository
	updateErr, deleteErr error
}

func (f *failingMeals) Update(ctx context.Context, m *meal.Meal) (bool, error) {
	if f.updateErr != nil {
		return false, f.updateErr
	}
	return f.MealRepository.Update(ctx, m)
}

func (f *failingMeals) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return f.MealRepository.DeleteByID(ctx, userID, id)
}

// failingWorkouts 同上，運動版.
type failingWorkouts struct {
	WorkoutStore
	updateErr, deleteErr error
}

func (f *failingWorkouts) Update(ctx context.Context, w *workout.Workout) (bool, error) {
	if f.updateErr != nil {
		return false, f.updateErr
	}
	return f.WorkoutStore.Update(ctx, w)
}

func (f *failingWorkouts) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return f.WorkoutStore.DeleteByID(ctx, userID, id)
}

func (f *editFixture) mealTotal() float64 { return f.meals.Items[0].Total.Calories }

// nonceOf 從按鈕 data 取出 nonce（ce1:<action>:<nonce>[:i]）.
func nonceOf(t *testing.T, data string) string {
	t.Helper()
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		t.Fatalf("not a button data: %q", data)
	}
	return parts[2]
}

// [F33-23] A14：按確認 → 寫入、回「已改成」（數字取自真的寫進去的紀錄）、暫存被取走；餐點與運動、改值 / 加料 / 刪除 都走同一條.
func TestConfirmAppliesEdit(t *testing.T) {
	t.Run("meal set item kcal", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "你要直接更新正確啦", planSetItem210)
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "已改成 210 大卡", "今日累計")
		if f.mealTotal() != 210 || f.meals.Items[0].Version != 1 {
			t.Errorf("stored total=%v version=%d, want 210/1", f.mealTotal(), f.meals.Items[0].Version)
		}
		if f.store.Peek("U1") != nil {
			t.Error("the pending must be taken")
		}
	})
	t.Run("meal add item", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "這裡面有起司，請重新分析", planAddCheese)
		mustContain(t, f.tapReply(t, "U1", ok), "已改成 190 大卡")
		if len(f.meals.Items[0].Items) != 2 || f.mealTotal() != 190 {
			t.Errorf("items=%d total=%v, want 2/190", len(f.meals.Items[0].Items), f.mealTotal())
		}
	})
	t.Run("meal delete", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "刪掉這餐", planDeleteMeal)
		mustContain(t, f.tapReply(t, "U1", ok), "已刪除")
		if len(f.meals.Items) != 0 {
			t.Errorf("meal still there: %+v", f.meals.Items)
		}
	})
	t.Run("workout duration recomputes calories", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedWorkout(t, time.Now().Add(-time.Hour))
		ok := f.proposeAndGetOK(t, "U1", "跑步改成 30 分鐘", planWorkout30)
		mustContain(t, f.tapReply(t, "U1", ok), "已更新", "30 分鐘", "今日運動消耗")
		w := f.workouts.Items[0]
		if w.DurationMin != 30 || w.Calories <= 314 || w.Version != 1 {
			t.Errorf("stored workout = %+v, want 30 min, more than 314 kcal, version 1", w)
		}
	})
	t.Run("workout delete", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedWorkout(t, time.Now().Add(-time.Hour))
		ok := f.proposeAndGetOK(t, "U1", "把這筆運動刪掉", planDelWorkout)
		mustContain(t, f.tapReply(t, "U1", ok), "已刪除")
		if len(f.workouts.Items) != 0 {
			t.Errorf("workout still there: %+v", f.workouts.Items)
		}
	})
}

// [F33-19] 按鈕的 postback 事件要被處理（以前被丟掉）；用事件裡的 userID.
func TestPostbackEventIsHandledNotDropped(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	before := len(f.api.replies)
	f.h.handleEventSafely(context.Background(), postbackEvent("U1", ok, time.Now()))
	if len(f.api.replies) != before+1 || f.mealTotal() != 210 {
		t.Errorf("postback dropped: replies %d -> %d, total=%v", before, len(f.api.replies), f.mealTotal())
	}
	// 群組 / 沒有 userId 的 postback 照舊被擋在 switch 之前.
	ev := postbackEvent("", ok, time.Now())
	ev.Source.Type = "group"
	f.h.handleEventSafely(context.Background(), ev)
	if len(f.api.replies) != before+1 {
		t.Error("a group postback must be ignored")
	}
}

// [F33-23] A15：同一個確認再按一次 → 已過期；資料只改一次（加料只加一次）.
func TestConfirmIsSingleUse(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "這裡面有起司", planAddCheese)
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 190")
	reply := f.tapReply(t, "U1", ok)
	mustContain(t, reply, "過期")
	assertNoSuccessClaim(t, reply)
	if len(f.meals.Items[0].Items) != 2 || f.mealTotal() != 190 || f.meals.Updates != 1 {
		t.Errorf("applied more than once: items=%d total=%v updates=%d", len(f.meals.Items[0].Items), f.mealTotal(), f.meals.Updates)
	}
}

// [F33-23] A15：多個 goroutine 同時送同一個 postback（-race）→ 恰好一次成功，其餘回過期；加料只加一次.
func TestConfirmConcurrentDoubleTapAppliesOnce(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "這裡面有起司", planAddCheese)
	ll := newLockedLine(t)
	f.h.line = ll.client

	const taps = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range taps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f.h.handleEventSafely(context.Background(), postbackEvent("U1", ok, time.Now()))
		}()
	}
	close(start)
	wg.Wait()

	var applied, expired int
	for _, r := range ll.all() {
		switch {
		case strings.Contains(r, "已改成"):
			applied++
		case strings.Contains(r, "過期"):
			expired++
		}
	}
	if applied != 1 || expired != taps-1 {
		t.Errorf("applied=%d expired=%d, want 1/%d", applied, expired, taps-1)
	}
	if len(f.meals.Items[0].Items) != 2 || f.mealTotal() != 190 || f.meals.Updates != 1 {
		t.Errorf("the cheese was added more than once: items=%d total=%v updates=%d", len(f.meals.Items[0].Items), f.mealTotal(), f.meals.Updates)
	}
}

// [F33-23] A16：提議後 5 分鐘（注入時鐘）再按確認 → 過期，不寫入.
func TestConfirmExpired(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	base := time.Now()
	f.setNow(base)
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	f.setNow(base.Add(recordedit.PendingTTL))
	reply := f.tapReply(t, "U1", ok)
	mustContain(t, reply, "過期")
	assertNoSuccessClaim(t, reply)
	f.assertNothingWritten(t, 1, 0)
}

// [F33-23] A16 反向：4 分 59 秒內按 → 成功.
func TestConfirmJustBeforeExpiry(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	base := time.Now()
	f.setNow(base)
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	f.setNow(base.Add(4*time.Minute + 59*time.Second))
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
}

// [F33-23] A17：按取消 → 「好，沒有修改」，零寫入，暫存清除.
func TestCancelWritesNothing(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	reply := f.tapReply(t, "U1", f.buttonData(t, "no"))
	mustContain(t, reply, "沒有修改")
	f.assertNothingWritten(t, 1, 0)
	if f.mealTotal() != 120 || f.store.Peek("U1") != nil {
		t.Errorf("total=%v pending=%v", f.mealTotal(), f.store.Peek("U1"))
	}
}

// [F33-23] A17 反向：取消後再按舊確認 → 過期，不寫入.
func TestConfirmAfterCancelRejected(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	no := f.buttonData(t, "no")
	f.tapReply(t, "U1", no)
	reply := f.tapReply(t, "U1", ok)
	mustContain(t, reply, "過期")
	assertNoSuccessClaim(t, reply)
	f.assertNothingWritten(t, 1, 0)
}

// [F33-23] A18：亂 nonce → 過期訊息、不寫入.
func TestPostbackTamperedNonceRejected(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	forged := "ce1:ok:" + strings.Repeat("A", recordedit.NonceLen)
	if forged == ok {
		t.Fatal("test nonce collides")
	}
	reply := f.tapReply(t, "U1", forged)
	mustContain(t, reply, "過期")
	assertNoSuccessClaim(t, reply)
	f.assertNothingWritten(t, 1, 0)
}

// [F33-23] A18：亂 nonce 不得消耗客人手上有效的提議（之後用正確 nonce 仍可確認）.
func TestPostbackTamperedNonceKeepsValidPending(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	for _, kind := range []string{"ok", "no", "pick"} {
		data := "ce1:" + kind + ":" + strings.Repeat("B", recordedit.NonceLen)
		if kind == "pick" {
			data += ":0"
		}
		f.tapReply(t, "U1", data)
	}
	if f.store.Peek("U1") == nil {
		t.Fatal("a wrong nonce consumed or deleted the valid pending")
	}
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
}

// [F33-19] A18：data 格式錯 / 超長 / 未知動作 → 靜默忽略（不回覆、不 panic、不碰暫存）.
func TestPostbackMalformedData(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	nonce := nonceOf(t, ok)
	for _, data := range []string{
		"", "garbage", "storeId=12345", "ce1", "ce1:ok", "ce1:ok:", "ce1:ok:short", "ce1:ok:" + nonce + ":extra",
		"ce2:ok:" + nonce, "ce1:evil:" + nonce, "ce1:pick:" + nonce, "ce1:pick:" + nonce + ":5", "ce1:pick:" + nonce + ":-1",
		"ce1:pick:" + nonce + ":abc", "ce1:pick:" + nonce + ":01", "ce1:ok:" + nonce + strings.Repeat("A", 60),
		strings.Repeat("a", 65), strings.Repeat("a", 5000), "ce1:ok:" + nonce[:21] + "!", "ce1:ok:" + nonce[:21] + "\x00",
	} {
		takes := f.store.Takes.Load()
		if n := f.tap("U1", data); n != 0 {
			t.Errorf("data %q got %d replies, want silence", data, n)
		}
		if f.store.Takes.Load() != takes {
			t.Errorf("data %q reached the store", data)
		}
	}
	if f.store.Peek("U1") == nil {
		t.Error("malformed postbacks touched the valid pending")
	}
	f.assertNothingWritten(t, 1, 0)
}

// [F33-19] A18：postback 不動對話狀態（例如正在追問餐點）、也不當成使用者資料寫入.
func TestPostbackDoesNotDisturbConversationState(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	state := &conversation.State{Type: conversation.TypeMealClarify, Meal: &conversation.PendingMeal{Text: "便當"}}
	if err := f.h.convManager.Set(context.Background(), "U1", state); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{f.buttonData(t, "no"), "garbage", ok} {
		f.tap("U1", data)
		got, err := f.h.convManager.Get(context.Background(), "U1")
		if err != nil || got == nil || got.Type != conversation.TypeMealClarify || got.Meal == nil || got.Meal.Text != "便當" {
			t.Fatalf("after postback %q the conversation state is %+v err=%v", data, got, err)
		}
	}
}

// [F33-23] A19：跨使用者——U2 送 U1 的 nonce 取不到（Store 以事件 userID 取），U1 的提議與紀錄都不變，U1 之後仍可確認.
func TestEditCrossUserRejected(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "U1的麵包", 120, time.Now().Add(-time.Minute))
	f.seedMeal(t, "U2", "U2的飯糰", 300, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)

	reply := f.tapReply(t, "U2", ok)
	mustContain(t, reply, "過期")
	assertNoSuccessClaim(t, reply)
	if f.store.Peek("U1") == nil {
		t.Fatal("U2's tap consumed U1's pending")
	}
	for _, m := range f.meals.Items {
		if m.Total.Calories == 210 {
			t.Fatalf("a record was changed by another user's tap: %+v", m)
		}
	}
	// U2 自己的提議不能被 U1 的 nonce 取走，各自獨立.
	ok2 := f.proposeAndGetOK(t, "U2", "把熱量改成 210", planSetItem210)
	if nonceOf(t, ok2) == nonceOf(t, ok) {
		t.Fatal("nonces must differ per proposal")
	}
	mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
	for _, m := range f.meals.Items {
		if m.UserID == "U1" && m.Total.Calories != 210 || m.UserID == "U2" && m.Total.Calories != 300 {
			t.Errorf("wrong record changed: %+v", m)
		}
	}
}

// [F33-23] A21：數值超範圍 → 不出確認卡、不存暫存，回含範圍的提示；邊界 3000 / 1 通過，3001 / 0 拒絕.
func TestEditOutOfRangeRejectedBeforeProposal(t *testing.T) {
	tests := []struct {
		name     string
		kcal     string
		wantCard bool
	}{
		{"99999", "99999", false},
		{"3001", "3001", false},
		{"3000 is the item maximum", "3000", true},
		{"1 is the item minimum", "1", true},
		{"0", "0", false},
		{"negative", "-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEditFixture(t)
			f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
			f.plan.answers = []string{`{"op":"set_item_kcal","target":"M1","item":1,"kcal":` + tt.kcal + `}`}
			reply := f.sayAs(t, "U1", "把熱量改成"+tt.kcal)
			hasCard := strings.Contains(reply, `"type":"postback"`)
			if hasCard != tt.wantCard {
				t.Fatalf("card = %v, want %v: %s", hasCard, tt.wantCard, reply)
			}
			if !tt.wantCard {
				if f.store.Puts.Load() != 0 {
					t.Error("an out-of-range value must not be stored")
				}
				assertNoSuccessClaim(t, reply)
			}
			f.assertNothingWritten(t, 1, 0)
		})
	}
	// 範圍提示原樣講給客人.
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	f.plan.answers = []string{`{"op":"set_item_kcal","target":"M1","item":1,"kcal":99999}`}
	mustContain(t, f.sayAs(t, "U1", "把熱量改成 99999"), "3000")
}

// [F33-23] A22：確認時寫入失敗 → 「修改失敗」，不含「已改」「已更新」「已刪除」；DB 不變.
func TestConfirmWriteFailureDoesNotClaimSuccess(t *testing.T) {
	boom := errors.New("mongo: write conflict org_secret123")
	tests := []struct {
		name string
		run  func(t *testing.T, f *editFixture) string
	}{
		{"meal update fails", func(t *testing.T, f *editFixture) string {
			f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
			ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
			f.h.mealRepo = &failingMeals{MealRepository: f.meals, updateErr: boom}
			return f.tapReply(t, "U1", ok)
		}},
		{"meal delete fails", func(t *testing.T, f *editFixture) string {
			f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
			ok := f.proposeAndGetOK(t, "U1", "刪掉這餐", planDeleteMeal)
			f.h.mealRepo = &failingMeals{MealRepository: f.meals, deleteErr: boom}
			return f.tapReply(t, "U1", ok)
		}},
		{"workout update fails", func(t *testing.T, f *editFixture) string {
			f.seedWorkout(t, time.Now().Add(-time.Hour))
			ok := f.proposeAndGetOK(t, "U1", "跑步改成 30 分鐘", planWorkout30)
			f.h.workoutRepo = &failingWorkouts{WorkoutStore: f.workouts, updateErr: boom}
			return f.tapReply(t, "U1", ok)
		}},
		{"workout delete fails", func(t *testing.T, f *editFixture) string {
			f.seedWorkout(t, time.Now().Add(-time.Hour))
			ok := f.proposeAndGetOK(t, "U1", "把這筆運動刪掉", planDelWorkout)
			f.h.workoutRepo = &failingWorkouts{WorkoutStore: f.workouts, deleteErr: boom}
			return f.tapReply(t, "U1", ok)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEditFixture(t)
			reply := tt.run(t, f)
			mustContain(t, reply, "修改失敗")
			assertNoSuccessClaim(t, reply)
			if strings.Contains(reply, "org_secret") || strings.Contains(reply, "mongo") {
				t.Errorf("error text leaked: %s", reply)
			}
			if len(f.meals.Items) > 0 && (f.mealTotal() != 120 || len(f.meals.Items[0].Items) != 1) {
				t.Errorf("meal changed: %+v", f.meals.Items[0])
			}
			if len(f.workouts.Items) > 0 && (f.workouts.Items[0].DurationMin != 20 || f.workouts.Items[0].Calories != 314) {
				t.Errorf("workout changed: %+v", f.workouts.Items[0])
			}
		})
	}
}

// [F33-23] A23：提議與確認之間紀錄被改過 → 「被改過」不套用（餐點與運動）；Update 回 ErrConflict 也一樣.
func TestConfirmStaleVersionRejected(t *testing.T) {
	t.Run("meal edited between proposal and confirm", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		f.meals.EditStored(0, func(m *meal.Meal) { m.Note = "LIFF 改過" })
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "被改過")
		assertNoSuccessClaim(t, reply)
		if f.mealTotal() != 120 {
			t.Errorf("a stale proposal was applied: total=%v", f.mealTotal())
		}
	})
	t.Run("workout edited between proposal and confirm", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedWorkout(t, time.Now().Add(-time.Hour))
		ok := f.proposeAndGetOK(t, "U1", "跑步改成 30 分鐘", planWorkout30)
		f.workouts.EditStored(0, func(w *workout.Workout) { w.Calories = 999 })
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "被改過")
		assertNoSuccessClaim(t, reply)
		if w := f.workouts.Items[0]; w.DurationMin != 20 || w.Calories != 999 {
			t.Errorf("a stale proposal was applied: %+v", w)
		}
	})
	t.Run("delete proposal is stale too", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "刪掉這餐", planDeleteMeal)
		f.meals.EditStored(0, func(m *meal.Meal) { m.Note = "改過" })
		mustContain(t, f.tapReply(t, "U1", ok), "被改過")
		if len(f.meals.Items) != 1 {
			t.Error("a stale delete proposal deleted the record")
		}
	})
	t.Run("changed after the read: Update reports a conflict", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		f.meals.BeforeUpdate = func(m *storagetest.Meals) {
			m.EditStored(0, func(x *meal.Meal) { x.Note = "同時被改" })
			f.meals.BeforeUpdate = nil
		}
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "被改過")
		assertNoSuccessClaim(t, reply)
	})
}

// [F33-23] A24：確認時紀錄已被刪 → 「這筆已經不在了」，不得說已改成.
func TestConfirmRecordGone(t *testing.T) {
	t.Run("meal", func(t *testing.T) {
		f := newEditFixture(t)
		m := f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		if deleted, err := f.meals.DeleteByID(context.Background(), "U1", m.ID); !deleted || err != nil {
			t.Fatal("setup: delete failed")
		}
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "不在了")
		if strings.Contains(reply, "已改成") {
			t.Errorf("claims a change on a deleted record: %s", reply)
		}
		if len(f.meals.Items) != 0 {
			t.Error("the deleted meal was revived")
		}
	})
	t.Run("workout", func(t *testing.T) {
		f := newEditFixture(t)
		w := f.seedWorkout(t, time.Now().Add(-time.Hour))
		ok := f.proposeAndGetOK(t, "U1", "跑步改成 30 分鐘", planWorkout30)
		if deleted, err := f.workouts.DeleteByID(context.Background(), "U1", w.ID); !deleted || err != nil {
			t.Fatal("setup: delete failed")
		}
		mustContain(t, f.tapReply(t, "U1", ok), "不在了")
		if len(f.workouts.Items) != 0 {
			t.Error("the deleted workout was revived")
		}
	})
	t.Run("delete proposal on an already deleted record", func(t *testing.T) {
		f := newEditFixture(t)
		m := f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "刪掉這餐", planDeleteMeal)
		if deleted, _ := f.meals.DeleteByID(context.Background(), "U1", m.ID); !deleted {
			t.Fatal("setup")
		}
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "不在了")
		assertNoSuccessClaim(t, reply)
	})
}

// [F33-23] A25：候選點選的 index 越界 → 忽略（不寫入、不出確認卡、不重新 Put）；格式不合的 index 被解析擋下，靜默.
func TestPickOutOfRangeIgnored(t *testing.T) {
	f := newEditFixture(t)
	now := time.Now()
	f.seedMeal(t, "U1", "全麥麵包", 150, now.Add(-time.Minute))
	f.seedMeal(t, "U1", "芝麻麵包", 220, now.Add(-2*time.Hour))
	f.plan.answers = []string{`{"op":"set_item_kcal","target":"","item":1,"kcal":100,"candidates":["M1","M2"]}`}
	f.sayAs(t, "U1", "麵包熱量改成 100")
	nonce := nonceOf(t, f.buttonData(t, "pick"))
	puts := f.store.Puts.Load()

	for _, data := range []string{"ce1:pick:" + nonce + ":5", "ce1:pick:" + nonce + ":-1", "ce1:pick:" + nonce + ":abc"} {
		if n := f.tap("U1", data); n != 0 {
			t.Errorf("unparseable index %q must be ignored silently, got %d replies", data, n)
		}
	}
	reply := f.tapReply(t, "U1", "ce1:pick:"+nonce+":4") // 格式合法但沒有第 5 個候選
	if strings.Contains(reply, `"type":"postback"`) {
		t.Errorf("an out-of-range pick produced a card: %s", reply)
	}
	assertNoSuccessClaim(t, reply)
	if f.store.Puts.Load() != puts {
		t.Error("an out-of-range pick must not store a new proposal")
	}
	f.assertNothingWritten(t, 2, 0)
}

// [F33-23] A25：點選候選後仍須再按確認才寫；寫的是選到的那一筆；候選在選擇前被刪 → 不在了.
func TestPickStillNeedsConfirm(t *testing.T) {
	setup := func(t *testing.T) *editFixture {
		f := newEditFixture(t)
		now := time.Now()
		f.seedMeal(t, "U1", "全麥麵包", 150, now.Add(-time.Minute))
		f.seedMeal(t, "U1", "芝麻麵包", 220, now.Add(-2*time.Hour))
		f.plan.answers = []string{`{"op":"set_item_kcal","target":"","item":1,"kcal":100,"candidates":["M1","M2"]}`}
		f.sayAs(t, "U1", "麵包熱量改成 100")
		return f
	}
	t.Run("pick then confirm writes the picked record only", func(t *testing.T) {
		f := setup(t)
		var second string
		for _, a := range f.quickReplies(t) {
			if d, _ := a["data"].(string); strings.HasSuffix(d, ":1") && strings.HasPrefix(d, "ce1:pick:") {
				second = d
			}
		}
		if second == "" {
			t.Fatalf("no second pick button: %s", f.api.lastReply(t))
		}
		reply := f.tapReply(t, "U1", second)
		mustContain(t, reply, "芝麻麵包", `"type":"postback"`)
		f.assertNothingWritten(t, 2, 0) // 選了還沒寫
		mustContain(t, f.tapReply(t, "U1", f.buttonData(t, "ok")), "已改成 100")
		for _, m := range f.meals.Items {
			switch m.Items[0].Name {
			case "芝麻麵包":
				if m.Total.Calories != 100 {
					t.Errorf("the picked meal was not changed: %+v", m)
				}
			case "全麥麵包":
				if m.Total.Calories != 150 {
					t.Errorf("the other meal was changed: %+v", m)
				}
			}
		}
	})
	t.Run("candidate deleted before the choice", func(t *testing.T) {
		f := setup(t)
		first := f.buttonData(t, "pick")
		var gone string
		for _, m := range f.meals.Items {
			if m.Items[0].Name == "全麥麵包" {
				gone = m.ID
			}
		}
		if ok, err := f.meals.DeleteByID(context.Background(), "U1", gone); !ok || err != nil {
			t.Fatal("setup: delete failed")
		}
		// buttonData 取到第一顆（M1 = 全麥麵包，已被刪）.
		reply := f.tapReply(t, "U1", first)
		mustContain(t, reply, "不在了")
		if strings.Contains(reply, `"type":"postback"`) {
			t.Errorf("a card for a deleted record: %s", reply)
		}
	})
}

// [F33-24] A28：刪除資料 → 暫存的提議一併清掉；別人的不動；後端刪除失敗要回報（不能假裝清了）.
func TestEraseUserDataClearsPendingEdit(t *testing.T) {
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
	f.seedMeal(t, "U2", "飯糰", 300, time.Now().Add(-time.Minute))
	f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
	f.proposeAndGetOK(t, "U2", "把熱量改成 210", planSetItem210)

	if failed := f.h.eraseUserData(context.Background(), "U1"); len(failed) != 0 {
		t.Fatalf("erase failed parts: %v", failed)
	}
	if f.store.Peek("U1") != nil {
		t.Error("U1's pending edit survived the erasure")
	}
	if f.store.Peek("U2") == nil {
		t.Error("erasing U1 removed U2's pending edit")
	}
	// 後端失敗要列為失敗的一項.
	f.store.DeleteErr = errors.New("redis down")
	failed := f.h.eraseUserData(context.Background(), "U2")
	found := false
	for _, p := range failed {
		found = found || p == "pending edit"
	}
	if !found {
		t.Errorf("a failed pending-edit delete must be reported: %v", failed)
	}
}

// [F33-23] A28：提議存好後客人刪光資料再按確認 → 不寫入、紀錄不復活（暫存已被清，回過期）；刪除標記晚於按鈕時間 → 回已刪除.
func TestEditConfirmAfterErasure(t *testing.T) {
	t.Run("erased then tapped", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		if err := f.h.EraseAll(context.Background(), "U1"); err != nil {
			t.Fatal(err)
		}
		reply := f.tapReply(t, "U1", ok)
		assertNoSuccessClaim(t, reply)
		if len(f.meals.Items) != 0 || f.meals.Updates != 0 {
			t.Errorf("a record was written or revived after the erasure: %+v", f.meals.Items)
		}
	})
	t.Run("erasure mark newer than the tap (pending survived)", func(t *testing.T) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		if err := f.h.erased.mark(context.Background(), "U1"); err != nil {
			t.Fatal(err)
		}
		f.h.handleEventSafely(context.Background(), postbackEvent("U1", ok, time.Now().Add(-time.Minute)))
		mustContain(t, f.api.lastReply(t), "刪除")
		if f.mealTotal() != 120 || f.meals.Updates != 0 {
			t.Errorf("written after the erasure mark: total=%v updates=%d", f.mealTotal(), f.meals.Updates)
		}
	})
}

// [F34-17] 取代 PR#33 的 TestTypedConfirmWordDoesNotApplyRecordEdit（安全設計依 user 決策反轉，見 dispatch Threat Surface）：
// 有待確認時打全等「確認」＝按確認鈕（Store.Take 呼叫 1 次、套用結果與按 postback 逐欄相同）；
// 「✅ 確認」「好」「ok」「確認！」「確認刪除」與 postback data 字串原文都不碰 Take、資料不變、暫存仍在.
func TestTypedConfirmEqualsButton(t *testing.T) {
	setup := func(t *testing.T) (*editFixture, string) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		ok := f.proposeAndGetOK(t, "U1", "把熱量改成 210", planSetItem210)
		return f, ok
	}

	t.Run("typed confirm applies the proposal exactly like the button", func(t *testing.T) {
		typed, _ := setup(t)
		takes := typed.store.Takes.Load()
		typed.quiet("U1", "確認")
		if got := typed.store.Takes.Load() - takes; got != 1 {
			t.Fatalf("Store.Take called %d times by the typed confirm, want 1", got)
		}
		typedReply := typed.api.lastReply(t)
		button, ok := setup(t)
		button.tapReply(t, "U1", ok)
		a, b := typed.meals.Items[0], button.meals.Items[0]
		if a.Total != b.Total || a.Version != b.Version || a.Items[0].Calories != b.Items[0].Calories || a.Items[0].ProteinG != b.Items[0].ProteinG ||
			a.MealType != b.MealType || a.Scale != b.Scale || len(a.Items) != len(b.Items) {
			t.Errorf("typed confirm result differs from the button's:\ntyped  %+v\nbutton %+v", a, b)
		}
		if a.Total.Calories != 210 || a.Version != 1 || typed.meals.Updates != 1 {
			t.Errorf("typed confirm: total=%v version=%d updates=%d, want 210 / 1 / 1", a.Total.Calories, a.Version, typed.meals.Updates)
		}
		mustContain(t, typedReply, "已改成 210")
		if typed.store.Len() != 0 {
			t.Error("the pending must be consumed by a typed confirm")
		}
	})

	for _, word := range []string{"✅ 確認", "好", "ok", "確認！", "確認刪除", "確認 好", "請確認"} {
		t.Run("not a confirm: "+word, func(t *testing.T) {
			f, _ := setup(t)
			takes := f.store.Takes.Load()
			f.quiet("U1", word)
			if f.store.Takes.Load() != takes {
				t.Errorf("%q reached Store.Take", word)
			}
			if f.mealTotal() != 120 || f.meals.Updates != 0 {
				t.Errorf("%q applied the proposal: total=%v updates=%d", word, f.mealTotal(), f.meals.Updates)
			}
			if f.store.Peek("U1") == nil {
				t.Errorf("%q consumed the pending", word)
			}
		})
	}

	t.Run("the postback data string typed as text is not a confirm", func(t *testing.T) {
		f, ok := setup(t)
		takes := f.store.Takes.Load()
		f.quiet("U1", ok)
		if f.store.Takes.Load() != takes || f.mealTotal() != 120 || f.store.Peek("U1") == nil {
			t.Errorf("typed postback data acted as a confirm: takes+%d total=%v", f.store.Takes.Load()-takes, f.mealTotal())
		}
		// 之後真的按鈕仍有效.
		mustContain(t, f.tapReply(t, "U1", ok), "已改成 210")
	})
}

// [F33-19] 真實資料：LINE 官方 Messaging API reference「Postback event」範例 JSON 原文（只把 data 換成本系統的按鈕資料）
// 經 processEvents 的 JSON 解析 → userID 取自 source.userId、nonce 取自 postback.data.
// 出處 https://developers.line.biz/en/reference/messaging-api/#postback-event（官方範例的 userId 是 "U91eeaf62d..."）.
const officialPostbackJSON = `{
    "destination": "xxxxxxxxxx",
    "events": [
        {
            "replyToken": "b60d432864f44d079f6d8efe86cf404b",
            "type": "postback",
            "mode": "active",
            "source": {
                "userId": "U91eeaf62d...",
                "type": "user"
            },
            "timestamp": 1513669370317,
            "webhookEventId": "01FZ74A0TDDPYRVKNK77XKC3ZR",
            "deliveryContext": {
                "isRedelivery": false
            },
            "postback": {
                "data": "storeId=12345",
                "params": {
                    "datetime": "2017-12-25T01:00"
                }
            }
        }
    ]
}`

func TestPostbackEventFromOfficialSample(t *testing.T) {
	const sampleUser = "U91eeaf62d..."
	f := newEditFixture(t)
	f.seedMeal(t, sampleUser, "麵包", 120, time.Now().Add(-time.Minute))
	f.seedMeal(t, "U-other", "別人的飯糰", 300, time.Now().Add(-time.Minute))
	ok := f.proposeAndGetOK(t, sampleUser, "把熱量改成 210", planSetItem210)
	body := strings.Replace(officialPostbackJSON, "storeId=12345", ok, 1)
	if body == officialPostbackJSON {
		t.Fatal("the sample's data placeholder was not found")
	}
	f.h.processEvents(context.Background(), []byte(body))
	for _, m := range f.meals.Items {
		switch m.UserID {
		case sampleUser:
			if m.Total.Calories != 210 {
				t.Errorf("the sample user's meal = %v, want 210 (userID must come from source.userId)", m.Total.Calories)
			}
		default:
			if m.Total.Calories != 300 {
				t.Errorf("another user's meal changed: %+v", m)
			}
		}
	}
	mustContain(t, f.api.lastReply(t), "已改成 210")
	// 原始範例（data=storeId=12345）不是本系統的格式：靜默忽略.
	before := len(f.api.replies)
	f.h.processEvents(context.Background(), []byte(officialPostbackJSON))
	if len(f.api.replies) != before {
		t.Error("the unmodified official sample (not our data format) must be ignored silently")
	}
}

// [F33-26] WithAssistant 要把 assistant 設成修改規劃器：gate 命中的訊息會呼叫到它；WithAssistant(nil) 不 panic 且照舊落回問答.
func TestWithAssistantSetsEditPlanner(t *testing.T) {
	build := func(t *testing.T) (*editFixture, *WebhookHandler) {
		f := newEditFixture(t)
		f.seedMeal(t, "U1", "麵包", 120, time.Now().Add(-time.Minute))
		h := NewWebhookHandler(f.api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager()).
			WithNutrition(nutrition.NewAnalyzer(f.ai), f.meals, nil).
			WithRecordEdit(f.store)
		h.editPlanner = nil // 證明下面是 WithAssistant 設的
		return f, h
	}
	t.Run("a real assistant becomes the planner", func(t *testing.T) {
		f, h := build(t)
		f.plan.answers = []string{planSetItem210}
		h.WithAssistant(assistant.New(&assistant.Config{Completer: f.plan, Meals: f.meals}), nil)
		if h.editPlanner == nil {
			t.Fatal("WithAssistant did not set the edit planner")
		}
		h.handleTextMessage(context.Background(), "U1", "把熱量改成 210", "tok")
		if f.plan.Calls() != 1 {
			t.Errorf("planner model calls = %d, want 1", f.plan.Calls())
		}
		mustContain(t, f.api.lastReply(t), `"type":"postback"`)
	})
	t.Run("nil assistant (typed nil) keeps the feature off and does not panic", func(t *testing.T) {
		f, h := build(t)
		var none *assistant.Assistant
		h.WithAssistant(none, nil)
		if h.editPlanner != nil {
			t.Fatal("a typed-nil assistant must not become the planner (it would look enabled and panic)")
		}
		h.handleTextMessage(context.Background(), "U1", "把熱量改成 210", "tok")
		if f.store.Puts.Load() != 0 || f.plan.Calls() != 0 {
			t.Errorf("puts=%d calls=%d, want 0/0", f.store.Puts.Load(), f.plan.Calls())
		}
		if len(f.api.replies) == 0 {
			t.Error("the message must fall through to the old flow and be answered")
		}
	})
}
