package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"fmt"
	"strings"
	"testing"
)

// snackMealJ 一個帶 snack_only 的 meal；snackOnly 是原樣放進 JSON 的片段（true / false / "yes" …）.
func snackMealJ(typ string, day int, snackOnly string, foods ...string) string {
	return fmt.Sprintf(`{"meal_type":%q,"day_offset":%d,"snack_only":%s,"foods":[%s]}`, typ, day, snackOnly, strings.Join(foods, ","))
}

const (
	drinkText      = "喝了一杯珍珠奶茶"
	snackClarifyAB = `{"is_food":true,"meal_type":"","clarify":{"question":"飲料差很多，是哪一種？","options":["珍珠奶茶","紅茶"]},"meals":[]}`
	snackClarifyBF = `{"is_food":true,"meal_type":"breakfast","clarify":{"question":"飲料差很多，是哪一種？","options":["珍珠奶茶","紅茶"]},"meals":[]}`
)

// snackMealsOf 取出已存的餐別序列.
func snackMealsOf(f *mealFixture) []string {
	out := make([]string, len(f.meals.Items))
	for i := range f.meals.Items {
		out[i] = f.meals.Items[i].MealType
	}
	return out
}

// TestUntypedDrinkAtEveningIsSnackAtNow [F34-80] 20:00 沒說餐別的飲料：點心，EatenAt 是現在 20:00（不是點心典型時間 15:30）.
func TestUntypedDrinkAtEveningIsSnackAtNow(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, drinkText)

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 20:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	mustContain(t, reply, "已記錄點心（文字估算，拍照會更準）")
}

// TestUntypedDrinkInMorningIsSnack [F34-80] 10:30 沒說餐別的飲料：點心（不是午餐），EatenAt 10:30.
func TestUntypedDrinkInMorningIsSnack(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 10:30")

	f.say(t, drinkText)

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 10:30" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
}

// TestStatedMealTypeBeatsSnackOnly [F34-80] 說了早餐就是早餐（既有典型時間 08:00），snack_only 不影響.
func TestStatedMealTypeBeatsSnackOnly(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("breakfast", 0, "true", foodJ("咖啡", 120))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "早餐喝了咖啡")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "breakfast" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 08:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	mustContain(t, reply, "已記錄早餐")
}

// TestMixedMealFollowsTime [F34-80] 三明治加拿鐵（snack_only false）依時間分：08:00 早餐.
func TestMixedMealFollowsTime(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("", 0, "false", foodJ("三明治", 350), foodJ("拿鐵", 150))))
	f.setNow(t, "2026-03-04 08:00")

	f.say(t, "吃了三明治和拿鐵")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "breakfast" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 08:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
}

// TestYesterdaySnackKeepsClock [F34-80] 昨天喝的飲料：點心，EatenAt 是昨天 20:00（不是昨天 15:30），回覆「已記錄昨天的點心」.
func TestYesterdaySnackKeepsClock(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("", -1, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "昨天喝了珍珠奶茶")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-03 20:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	mustContain(t, reply, "已記錄昨天的點心")
}

// TestTodayOrderPutsEveningDrinkLast [F34-79][F34-80] 今天已有午餐 12:30、晚餐 18:30，再記 20:00 的飲料：今日飲食行序 午餐、晚餐、點心.
func TestTodayOrderPutsEveningDrinkLast(t *testing.T) {
	f := newMealFixture(t,
		multiJ(mealJ("lunch", 0, foodJ("便當", 700))),
		multiJ(mealJ("dinner", 0, foodJ("拉麵", 600))),
		multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))),
	)
	f.setNow(t, "2026-03-04 20:00")
	f.say(t, "午餐吃了便當")
	f.say(t, "晚餐吃了拉麵")
	f.say(t, drinkText)

	reply := f.say(t, "今日飲食")

	lunch, dinner, snack := strings.Index(reply, "午餐 便當"), strings.Index(reply, "晚餐 拉麵"), strings.Index(reply, "點心 珍珠奶茶")
	if lunch < 0 || dinner < 0 || snack < 0 || lunch >= dinner || dinner >= snack {
		t.Fatalf("order must be 午餐, 晚餐, 點心 (idx %d %d %d):\n%s", lunch, dinner, snack, reply)
	}
	if strings.Contains(reply, "20:00") || strings.Contains(reply, "12:30") {
		t.Errorf("today's list must not show clock times:\n%s", reply)
	}
}

// TestClarifyThenDrinkIsSnack [F34-80] 追問時沒說餐別，答完後重估為 snack_only → 點心.
func TestClarifyThenDrinkIsSnack(t *testing.T) {
	f := newMealFixture(t, snackClarifyAB, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 20:00")

	f.say(t, "喝了飲料")
	if len(f.meals.Items) != 0 {
		t.Fatal("nothing is saved before the answer")
	}
	f.say(t, "珍珠奶茶")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 20:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
}

// TestClarifyKeepsStatedTypeOverSnackOnly [F34-80] 追問前說了早餐：重估即使 snack_only 為 true，仍記早餐.
func TestClarifyKeepsStatedTypeOverSnackOnly(t *testing.T) {
	f := newMealFixture(t, snackClarifyBF, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 20:00")

	f.say(t, "早餐喝了飲料")
	f.say(t, "珍珠奶茶")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "breakfast" {
		t.Fatalf("pending breakfast must win over snack_only: %+v", f.meals.Items)
	}
}

// TestSnackOnlyAndStatedDinnerAreTwoRecords [F34-80] 14:00 一則訊息：沒說餐別的珍奶 + 說了晚餐的拉麵 → 兩筆，順序點心、晚餐.
func TestSnackOnlyAndStatedDinnerAreTwoRecords(t *testing.T) {
	f := newMealFixture(t, multiJ(
		snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440)),
		snackMealJ("dinner", 0, "false", foodJ("拉麵", 600)),
	))
	f.setNow(t, "2026-03-04 14:00")

	f.say(t, "喝了一杯珍奶，晚餐吃了拉麵")

	got := snackMealsOf(f)
	if len(got) != 2 || got[0] != "snack" || got[1] != "dinner" {
		t.Fatalf("meal types = %v, items = %+v", got, f.meals.Items)
	}
}

// photoSnackAnalysis 照片測試用：一杯飲料.
func photoSnackAnalysis(snackOnly bool) *nutrition.ImageAnalysis {
	food := meal.FoodItem{Name: "珍珠奶茶", Nutrients: meal.Nutrients{Calories: 440}}
	return &nutrition.ImageAnalysis{
		Kind: nutrition.KindFood, Foods: []meal.FoodItem{food}, Total: food.Nutrients, SnackOnly: snackOnly,
	}
}

// TestPhotoSnackOnlyIsSnackAtNow [F34-81] 沒說餐別的飲料照：點心，EatenAt 是釘住的現在 20:00（不是 15:30）；snack_only 為 false 時依時間（20:00 晚餐）.
func TestPhotoSnackOnlyIsSnackAtNow(t *testing.T) {
	t.Run("snack only", func(t *testing.T) {
		f := newMealFixture(t)
		f.setNow(t, "2026-03-04 20:00")

		f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(true), nil)

		if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 20:00" {
			t.Fatalf("meals = %+v", f.meals.Items)
		}
		mustContain(t, f.replyText(t), "已記錄點心")
	})
	t.Run("not snack only follows the clock", func(t *testing.T) {
		f := newMealFixture(t)
		f.setNow(t, "2026-03-04 20:00")

		f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(false), nil)

		if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "dinner" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 20:00" {
			t.Fatalf("meals = %+v", f.meals.Items)
		}
		if got := f.meals.Items[0].MealType; got != string(nutrition.MealTypeAt(f.meals.Items[0].EatenAt)) {
			t.Errorf("MealType %q must equal MealTypeAt(EatenAt)", got)
		}
	})
}

// TestPhotoStatedTypeBeatsSnackOnly [F34-81] 打字描述說了午餐：照片記午餐（photo_text），即使 snack_only 為 true.
func TestPhotoStatedTypeBeatsSnackOnly(t *testing.T) {
	f := newMealFixture(t)
	f.setNow(t, "2026-03-04 20:00")
	pending := &conversation.PendingMeal{MealType: "lunch", Text: "午餐喝珍奶"}

	f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(true), pending)

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "lunch" || f.meals.Items[0].Source != meal.SourcePhotoText {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
}

// TestPhotoYesterdaySnackKeepsClock [F34-81] 說了昨天、沒說餐別的飲料照：點心，EatenAt 昨天 20:00，回覆「已記錄昨天的點心」.
func TestPhotoYesterdaySnackKeepsClock(t *testing.T) {
	f := newMealFixture(t)
	f.setNow(t, "2026-03-04 20:00")
	pending := &conversation.PendingMeal{MealType: "", DayOffset: -1, Text: "昨天喝的"}

	f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(true), pending)

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-03 20:00" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	mustContain(t, f.replyText(t), "已記錄昨天的點心")
}
