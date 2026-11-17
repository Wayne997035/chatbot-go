package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"fmt"
	"testing"
	"time"
)

// planAddFilling 客人說餐包裡有包起司鮮奶油餡 → 新增品項 250 大卡.
const planAddFilling = `{"op":"add_item","target":"M1","new_item":{"name":"起司鮮奶油餡","portion":"1份","calories":250,"protein_g":5,"carbs_g":10,"fat_g":20},"basis":"客人說餐包裡有包起司鮮奶油餡"}`

// planHalfBun 客人說餐包只吃一半 → 該品項改成 75 大卡.
const planHalfBun = `{"op":"set_item_kcal","target":"M1","item":1,"kcal":75,"basis":"客人說只吃一半"}`

// seedTodayMeals 昨天一餐（不得算進今天）、今天便當 500、最新的餐包 150（M1）、別人的一餐（不得算進來）.
func seedTodayMeals(t *testing.T, f *editFixture) {
	t.Helper()
	now := time.Now()
	todayStart, _ := nutrition.DayRange(now)
	f.seedMeal(t, "U1", "牛肉麵", 800, todayStart.Add(-2*time.Hour))
	f.seedMeal(t, "U1", "便當", 500, now.Add(-2*time.Minute))
	f.seedMeal(t, "U1", "餐包", 150, now.Add(-time.Minute))
	f.seedMeal(t, "U2", "別人的早餐", 999, now.Add(-time.Minute))
}

// mealKcal 使用者 userID 名為 name 的那餐目前總熱量；找不到回 -1.
func mealKcal(f *editFixture, userID, name string) float64 {
	for i := range f.meals.Items {
		m := &f.meals.Items[i]
		if m.UserID == userID && len(m.Items) > 0 && m.Items[0].Name == name {
			return m.Total.Calories
		}
	}
	return -1
}

// qaSeesToday 改完後問答拿到的「使用者資料」：用同一份紀錄建問答，抓送給 AI 的請求內容.
func qaSeesToday(t *testing.T, f *editFixture) string {
	t.Helper()
	f.plan.mu.Lock()
	f.plan.answers = append(f.plan.answers, "你今天攝取的熱量如上。")
	f.plan.mu.Unlock()
	a := assistant.New(&assistant.Config{Completer: f.plan, Meals: f.meals, Workouts: f.workouts})
	if _, err := a.Answer(context.Background(), "U1", "今天吃了多少"); err != nil {
		t.Fatalf("問答失敗：%v", err)
	}
	f.plan.mu.Lock()
	defer f.plan.mu.Unlock()
	return fmt.Sprintf("%+v", f.plan.last)
}

// TestConfirmReplyShowsCorrectedTodayTotals [F33-41] user：「更新後得馬上查證 本日消耗 看有沒有更正」
// 「如果沒更新 騙使用者有更新 這些都是不行的要測出來」.
// 按確認後回覆的今日累計 = 今天其他餐 + 改過的那餐（昨天與別人的不算）；按確認前資料不變、卡片不宣稱已改；
// 改完問答看到的今日攝取也是新數字.
func TestConfirmReplyShowsCorrectedTodayTotals(t *testing.T) {
	t.Run("add filling raises today total", func(t *testing.T) {
		f := newEditFixture(t)
		seedTodayMeals(t, f)
		ok := f.proposeAndGetOK(t, "U1", "餐包裡面其實有包起司鮮奶油餡，幫我更新熱量", planAddFilling)
		card := f.api.lastReply(t)
		mustContain(t, card, "150", "400")
		assertNoSuccessClaim(t, card)
		if got := mealKcal(f, "U1", "餐包"); got != 150 {
			t.Fatalf("按確認前紀錄就變了：%v", got)
		}
		reply := f.tapReply(t, "U1", ok)
		mustContain(t, reply, "已改成 400 大卡", "今日累計：900 大卡（2 餐）")
		if got := mealKcal(f, "U1", "餐包"); got != 400 {
			t.Errorf("餐包存的是 %v，要 400", got)
		}
		if got := mealKcal(f, "U2", "別人的早餐"); got != 999 {
			t.Errorf("別人的紀錄被動到：%v", got)
		}
		mustContain(t, qaSeesToday(t, f), "今天：攝取 900 大卡（2 餐")
	})
	t.Run("half portion lowers today total", func(t *testing.T) {
		f := newEditFixture(t)
		seedTodayMeals(t, f)
		ok := f.proposeAndGetOK(t, "U1", "餐包我只吃了一半，幫我調整", planHalfBun)
		mustContain(t, f.tapReply(t, "U1", ok), "已改成 75 大卡", "今日累計：575 大卡（2 餐）")
		mustContain(t, qaSeesToday(t, f), "今天：攝取 575 大卡（2 餐")
	})
	t.Run("cancel keeps totals and claims nothing", func(t *testing.T) {
		f := newEditFixture(t)
		seedTodayMeals(t, f)
		f.proposeAndGetOK(t, "U1", "餐包裡面其實有包起司鮮奶油餡，幫我更新熱量", planAddFilling)
		reply := f.tapReply(t, "U1", f.buttonData(t, "no"))
		mustContain(t, reply, "沒有修改")
		assertNoSuccessClaim(t, reply)
		if got := mealKcal(f, "U1", "餐包"); got != 150 {
			t.Errorf("取消後紀錄變了：%v", got)
		}
		mustContain(t, qaSeesToday(t, f), "今天：攝取 650 大卡（2 餐")
	})
	t.Run("workout duration updates today burned", func(t *testing.T) {
		f := newEditFixture(t)
		now := time.Now()
		todayStart, _ := nutrition.DayRange(now)
		for _, w := range []*workout.Workout{
			{UserID: "U1", Activity: "瑜伽", DurationMin: 30, Calories: 100, Source: workout.SourceText, DoneAt: now.Add(-2 * time.Minute), CreateTime: now},
			{UserID: "U1", Activity: "游泳", DurationMin: 60, Calories: 500, Source: workout.SourceText, DoneAt: todayStart.Add(-time.Hour), CreateTime: now},
		} {
			if err := f.workouts.Insert(context.Background(), w); err != nil {
				t.Fatal(err)
			}
		}
		f.seedWorkout(t, now.Add(-time.Minute)) // 跑步 20 分 314 大卡，最新 = W1
		ok := f.proposeAndGetOK(t, "U1", "跑步改成 30 分鐘", planWorkout30)
		reply := f.tapReply(t, "U1", ok)
		var run float64
		for i := range f.workouts.Items {
			if f.workouts.Items[i].Activity == "跑步" {
				run = f.workouts.Items[i].Calories
			}
		}
		if run <= 314 {
			t.Fatalf("跑步熱量沒有重算：%v", run)
		}
		mustContain(t, reply, fmt.Sprintf("今日運動消耗：%.0f 大卡（2 筆）", run+100))
		mustContain(t, qaSeesToday(t, f), fmt.Sprintf("運動消耗 %.0f 大卡", run+100))
	})
}
