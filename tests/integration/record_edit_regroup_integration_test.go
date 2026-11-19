package integration

import (
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F34-12][F34-15] 改餐別 / 移品項 / 拆多餐在真的 MongoDB（testcontainers）上的整條路徑：
// webhook → 真的規劃器（只有模型呼叫是假的）→ BuildRegroupProposal → MongoDB 暫存 → 打字「確認」/ 按鈕 → Insert / Update / DeleteIfVersion.
// 時鐘不控（真實時間）：新開餐點的 EatenAt 只斷言落在台北今天（dispatch「時鐘」條 ③）.
// 固定資料 F：U1 今天晚餐 18:30、五項 450/200/300/500/360，合計 1810；預期值皆由品項熱量相加推得.

func itToday(h, m int) time.Time {
	n := time.Now().In(nutrition.Taipei)
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, nutrition.Taipei)
}

func (r *editRig) seedF(t *testing.T) *meal.Meal {
	t.Helper()
	names := []string{"牛肉火腿起司三明治", "沙拉", "雞胸肉", "飯", "湯"}
	kcal := []float64{450, 200, 300, 500, 360}
	m := &meal.Meal{UserID: "U1", EatenAt: itToday(18, 30), CreateTime: itToday(18, 30), MealType: "dinner", Source: meal.SourceText}
	for i := range names {
		m.Items = append(m.Items, meal.FoodItem{Name: names[i], Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal[i]}})
	}
	m.Recalc()
	if err := r.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (r *editRig) seedSlot(t *testing.T, userID, mealType, name string, at time.Time, kcal float64) *meal.Meal {
	t.Helper()
	m := &meal.Meal{
		UserID: userID, EatenAt: at, CreateTime: at, MealType: mealType, Source: meal.SourceText,
		Items: []meal.FoodItem{{Name: name, Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal}}},
	}
	m.Recalc()
	if err := r.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// todayShape U1 今天全部餐點的「餐別:合計/品項數」，排序後以逗號串接，並檢查每筆 EatenAt 都落在台北今天.
func (r *editRig) todayShape(t *testing.T) string {
	t.Helper()
	from, to := nutrition.DayRange(time.Now())
	ms, err := r.meals.FindByUserBetween(context.Background(), "U1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 0, len(ms))
	for i := range ms {
		parts = append(parts, fmt.Sprintf("%s:%.0f/%d", ms[i].MealType, ms[i].Total.Calories, len(ms[i].Items)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// replyTexts 把 LINE 回覆 JSON 的第一則訊息文字取出來（換行保留）.
func replyTexts(t *testing.T, replies []string) []string {
	t.Helper()
	out := make([]string, 0, len(replies))
	for _, rep := range replies {
		var payload struct {
			Messages []struct {
				Text string `json:"text"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(rep), &payload); err != nil || len(payload.Messages) == 0 {
			t.Fatalf("bad reply %q: %v", rep, err)
		}
		out = append(out, payload.Messages[0].Text)
	}
	return out
}

const (
	itMoveSandwich = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"}]}`
	itMoveChicken  = `{"op":"move_items","target":"M1","moves":[{"items":[3],"meal_type":"lunch"}]}`
	itSplitDinner  = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2,3],"meal_type":"lunch"},{"items":[4,5],"meal_type":"dinner"}]}`
	itSplitSnack   = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2,3],"meal_type":"lunch"},{"items":[4,5],"meal_type":"snack"}]}`
	itWholeToBf    = `{"op":"set_meal_type","target":"M1","meal_type":"breakfast"}`
)

// TestRegroupFlowOnMongo 規格 #2 與 #3 在真 Mongo 走完一次（出卡 → MongoDB 暫存 → 打字「確認」→ 寫入），再加 #1（原地改）與 #5（刪來源），斷言 DB 內容.
func TestRegroupFlowOnMongo(t *testing.T) {
	t.Run("#2 move one item to a new meal, typed confirm", func(t *testing.T) {
		r := newEditRig(t)
		src := r.seedF(t)
		r.plan.answers = []string{itMoveSandwich}
		r.say(t, "U1", "把牛肉火腿起司三明治移到早餐")
		card := replyTexts(t, r.waitReplies(t, 1))[0]
		for _, want := range []string{"・早餐：「牛肉火腿起司三明治」→ 新增 450 大卡", "・晚餐：剩 1360 大卡（原 1810）"} {
			if !strings.Contains(card, want) {
				t.Errorf("card %q is missing %q", card, want)
			}
		}
		if got := r.todayShape(t); got != "dinner:1810/5" {
			t.Fatalf("written before the confirmation: %s", got)
		}
		// 另一位使用者打「確認」：看不到別人的卡（MongoDB 暫存以 userID 為鍵），也不會消耗它.
		r.say(t, "U2", "確認")
		if other := replyTexts(t, r.waitReplies(t, 2))[1]; other != "目前沒有等你確認的修改" {
			t.Errorf("U2's typed confirm = %q", other)
		}
		if got := r.todayShape(t); got != "dinner:1810/5" {
			t.Fatalf("U2's confirm changed U1's data: %s", got)
		}
		r.say(t, "U1", "確認")
		reply := replyTexts(t, r.waitReplies(t, 3))[2]
		if !strings.HasSuffix(reply, "\n今日累計：1810 大卡（2 餐）") {
			t.Errorf("reply = %q", reply)
		}
		if got := r.todayShape(t); got != "breakfast:450/1,dinner:1360/4" {
			t.Fatalf("db = %s", got)
		}
		if d, _ := r.meals.FindByID(context.Background(), "U1", src.ID); d == nil || d.Version != src.Version+1 {
			t.Errorf("source = %+v, want the same ID with version+1", d)
		}
		// 暫存用掉了：再打「確認」→ 沒有待確認，不會寫第二次.
		r.say(t, "U1", "確認")
		if again := replyTexts(t, r.waitReplies(t, 4))[3]; again != "目前沒有等你確認的修改" {
			t.Errorf("second typed confirm = %q", again)
		}
		if got := r.todayShape(t); got != "breakfast:450/1,dinner:1360/4" {
			t.Errorf("db after a second confirm = %s", got)
		}
	})

	t.Run("#3 merge into the existing lunch, typed confirm", func(t *testing.T) {
		r := newEditRig(t)
		r.seedF(t)
		lunch := r.seedSlot(t, "U1", "lunch", "既有午餐便當", itToday(12, 0), 600)
		r.plan.answers = []string{itMoveChicken}
		r.say(t, "U1", "把雞胸肉移到午餐")
		card := replyTexts(t, r.waitReplies(t, 1))[0]
		for _, want := range []string{"・午餐：「雞胸肉」→ 併入原有 600 → 900 大卡", "・晚餐：剩 1510 大卡（原 1810）"} {
			if !strings.Contains(card, want) {
				t.Errorf("card %q is missing %q", card, want)
			}
		}
		r.say(t, "U1", "確認")
		reply := replyTexts(t, r.waitReplies(t, 2))[1]
		if !strings.HasSuffix(reply, "（2 餐）") {
			t.Errorf("reply = %q, merging must not change the meal count", reply)
		}
		if got := r.todayShape(t); got != "dinner:1510/4,lunch:900/2" {
			t.Fatalf("db = %s, want lunch 900 (merged) and dinner 1510 with no third record", got)
		}
		if l, _ := r.meals.FindByID(context.Background(), "U1", lunch.ID); l == nil || l.Version != lunch.Version+1 {
			t.Errorf("merged lunch = %+v, want the same ID with version+1", l)
		}
	})

	t.Run("#1 whole meal to breakfast, button confirm, changed in place", func(t *testing.T) {
		r := newEditRig(t)
		src := r.seedF(t)
		r.plan.answers = []string{itWholeToBf}
		r.say(t, "U1", "這筆改成早餐")
		ok := okData(t, r.waitReplies(t, 1)[0])
		r.tap(t, "U1", ok)
		replyTexts(t, r.waitReplies(t, 2))
		if got := r.todayShape(t); got != "breakfast:1810/5" {
			t.Fatalf("db = %s", got)
		}
		if m, _ := r.meals.FindByID(context.Background(), "U1", src.ID); m == nil || m.MealType != "breakfast" || m.Version != src.Version+1 {
			t.Errorf("record = %+v, want the same ID as breakfast", m)
		}
	})

	t.Run("#5 split into three and the emptied source is deleted, button confirm", func(t *testing.T) {
		r := newEditRig(t)
		src := r.seedF(t)
		r.plan.answers = []string{itSplitSnack}
		r.say(t, "U1", "幫我分成早午點心三餐")
		ok := okData(t, r.waitReplies(t, 1)[0])
		r.tap(t, "U1", ok)
		reply := replyTexts(t, r.waitReplies(t, 2))[1]
		if !strings.HasSuffix(reply, "\n今日累計：1810 大卡（3 餐）") {
			t.Errorf("reply = %q", reply)
		}
		if got := r.todayShape(t); got != "breakfast:450/1,lunch:500/2,snack:860/2" {
			t.Fatalf("db = %s", got)
		}
		if d, _ := r.meals.FindByID(context.Background(), "U1", src.ID); d != nil {
			t.Errorf("the emptied source is still there: %+v", d)
		}
	})
}

// nthUpdateFault 讓第 failAt 次 Update 回指定結果（其餘交給真的 MongoDB 儲存）.
type nthUpdateFault struct {
	meal.MealRepository
	mu     sync.Mutex
	n      int
	failAt int
	ok     bool
	err    error
}

func (f *nthUpdateFault) Update(ctx context.Context, m *meal.Meal) (bool, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	if n == f.failAt {
		return f.ok, f.err
	}
	return f.MealRepository.Update(ctx, m)
}

func assertNoSuccessWords(t *testing.T, reply string) {
	t.Helper()
	for _, bad := range []string{"已調整", "已改", "已更新", "今日累計"} {
		if strings.Contains(reply, bad) {
			t.Errorf("failure reply contains %q: %s", bad, reply)
		}
	}
}

// TestRegroupRollbackOnMongo 規格 #12：W1、W2（兩筆新紀錄）Insert 成功後來源 Update 回錯 → 兩筆新紀錄被刪、D 不變、回覆不含已調整 / 已改、沒有今日累計.
func TestRegroupRollbackOnMongo(t *testing.T) {
	var fault *nthUpdateFault
	r := newEditRigWith(t, func(real *meal.MealRepo) meal.MealRepository {
		fault = &nthUpdateFault{MealRepository: real, failAt: 1, err: errors.New("injected: source update failed")}
		return fault
	})
	src := r.seedF(t)
	r.plan.answers = []string{itSplitDinner}
	r.say(t, "U1", "幫我分成早午晚三餐")
	ok := okData(t, r.waitReplies(t, 1)[0])
	r.tap(t, "U1", ok)
	reply := replyTexts(t, r.waitReplies(t, 2))[1]

	if !strings.Contains(reply, "修改失敗") {
		t.Errorf("reply = %q, want the failure message", reply)
	}
	assertNoSuccessWords(t, reply)
	if got := r.todayShape(t); got != "dinner:1810/5" {
		t.Fatalf("db = %s, want only the untouched source (both inserted records must be gone)", got)
	}
	d, _ := r.meals.FindByID(context.Background(), "U1", src.ID)
	if d == nil || d.Version != src.Version || len(d.Items) != 5 || d.Total.Calories != 1810 {
		t.Errorf("source = %+v, want it unchanged", d)
	}
}

// TestRegroupMergeRollbackOnMongo 規格 #13：併入目標 L 的 Update 成功（600 → 900）後來源 Update 回 ErrConflict
// → L 被寫回 600、沒有新紀錄、回 editStaleMessage.
func TestRegroupMergeRollbackOnMongo(t *testing.T) {
	r := newEditRigWith(t, func(real *meal.MealRepo) meal.MealRepository {
		return &nthUpdateFault{MealRepository: real, failAt: 2, err: meal.ErrConflict} // 第 1 次是 L，第 2 次是來源
	})
	r.seedF(t)
	lunch := r.seedSlot(t, "U1", "lunch", "既有午餐便當", itToday(12, 0), 600)
	r.plan.answers = []string{itMoveChicken}
	r.say(t, "U1", "把雞胸肉移到午餐")
	ok := okData(t, r.waitReplies(t, 1)[0])
	r.tap(t, "U1", ok)
	reply := replyTexts(t, r.waitReplies(t, 2))[1]

	if reply != "這筆在你確認前被改過，請重新說一次" {
		t.Errorf("reply = %q, want the stale message", reply)
	}
	assertNoSuccessWords(t, reply)
	if got := r.todayShape(t); got != "dinner:1810/5,lunch:600/1" {
		t.Fatalf("db = %s, want lunch written back to 600 and no new record", got)
	}
	l, _ := r.meals.FindByID(context.Background(), "U1", lunch.ID)
	if l == nil || len(l.Items) != 1 || l.Items[0].Name != "既有午餐便當" || l.Total.Calories != 600 {
		t.Errorf("lunch = %+v, want the original content back", l)
	}
}
