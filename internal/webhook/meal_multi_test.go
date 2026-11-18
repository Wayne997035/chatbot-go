package webhook

import (
	"chatbot-go/internal/intent"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// screenshotText 是正式環境客訴截圖原文（逐字）.
const screenshotText = "早餐吃 牛肉火腿起司三明治\n\n午餐 黑麥酸種麵包兩個手掌大小\n\n晚餐 福隆雞腿便當 加上哈根達斯草莓口味4匙 加上一杯牛奶"

func foodJ(name string, cal int) string {
	return fmt.Sprintf(`{"name":%q,"portion":"1份","calories":%d}`, name, cal)
}

func mealJ(typ string, day int, foods ...string) string {
	return fmt.Sprintf(`{"meal_type":%q,"day_offset":%d,"foods":[%s]}`, typ, day, strings.Join(foods, ","))
}

func multiJ(meals ...string) string {
	return `{"is_food":true,"meals":[` + strings.Join(meals, ",") + `]}`
}

// shotJSON 截圖原文的 AI 回答：早/午/晚三餐，晚餐 4 個品項；總熱量 350+200+1130 = 1680（取自這份假資料，不取自實作輸出）.
var shotJSON = multiJ(
	mealJ("breakfast", 0, foodJ("牛肉火腿起司三明治", 350)),
	mealJ("lunch", 0, foodJ("黑麥酸種麵包", 200)),
	mealJ("dinner", 0, foodJ("福隆雞腿便當", 450), foodJ("白飯", 280), foodJ("哈根達斯草莓冰淇淋", 250), foodJ("牛奶", 150)),
)

// setNow 釘住 handler 與 recent 指標的時鐘（兩者在正式環境都是 time.Now）；ts 為台灣時間 "2006-01-02 15:04"，可帶秒 "…:05".
func (f *mealFixture) setNow(t *testing.T, ts string) {
	t.Helper()
	layout := "2006-01-02 15:04"
	if strings.Count(ts, ":") == 2 {
		layout = "2006-01-02 15:04:05"
	}
	tm, err := time.ParseInLocation(layout, ts, nutrition.Taipei)
	if err != nil {
		t.Fatal(err)
	}
	f.h.clock = func() time.Time { return tm }
	f.h.recent.Now = func() time.Time { return tm }
}

// taipeiAt 紀錄的用餐時間（台灣時間）.
func taipeiAt(m *meal.Meal) string { return m.EatenAt.In(nutrition.Taipei).Format("2006-01-02 15:04") }

func (f *mealFixture) replyText(t *testing.T) string {
	t.Helper()
	return replyTextOf(t, f.api.lastReply(t))
}

// newMealIntentFixture 打字記餐加上意圖分類器（意圖 AI 固定回 intentAnswer）.
func newMealIntentFixture(t *testing.T, intentAnswer string, texts ...string) (*mealFixture, *intentCompleter) {
	t.Helper()
	f := newMealFixture(t, texts...)
	ic := &intentCompleter{answer: intentAnswer, t: t}
	f.h.WithIntent(intent.New(ic))
	return f, ic
}

// TestScreenshotThreeMealsAreThreeRecords [F34-22] 截圖原文必須記成早/午/晚三筆（各自餐別與時間）；正式環境原樣是 1 筆 18:30 的晚餐.
func TestScreenshotThreeMealsAreThreeRecords(t *testing.T) {
	f := newMealFixture(t, shotJSON)
	f.setNow(t, "2026-03-04 20:00")

	f.say(t, screenshotText)

	if len(f.meals.Items) != 3 {
		t.Fatalf("meals = %d, want 3: %+v", len(f.meals.Items), f.meals.Items)
	}
	wantType := []string{"breakfast", "lunch", "dinner"}
	wantAt := []string{"2026-03-04 08:00", "2026-03-04 12:30", "2026-03-04 18:30"}
	for i, m := range f.meals.Items {
		if m.MealType != wantType[i] || taipeiAt(&m) != wantAt[i] {
			t.Errorf("meal %d = %s at %s, want %s at %s", i, m.MealType, taipeiAt(&m), wantType[i], wantAt[i])
		}
	}
	if got := len(f.meals.Items[2].Items); got != 4 {
		t.Errorf("the dinner record carries only dinner's 4 foods, got %d", got)
	}
	if got := f.meals.Items[0].Total.Calories + f.meals.Items[1].Total.Calories; got != 550 {
		t.Errorf("breakfast+lunch = %v, want 550 (dinner foods must not leak into them)", got)
	}
	text := f.replyText(t)
	if !strings.Contains(text, "共 3 餐") || strings.Contains(text, "（1 餐）") {
		t.Errorf("reply: %s", text)
	}
	if lines := strings.Split(text, "\n"); lines[len(lines)-1] != "今日累計：1680 大卡（3 餐）" {
		t.Errorf("last line = %q, want 今日累計：1680 大卡（3 餐）", lines[len(lines)-1])
	}
	if len(f.ai.reqs) != 1 {
		t.Errorf("estimate calls = %d, want 1", len(f.ai.reqs))
	}
}

// TestMorningNoonEveningRuleRoute [F34-22] 「早上吃了蛋餅，中午吃了便當，晚上吃了滷肉飯」走規則：三筆，意圖 AI 0 次、問答 0 次.
func TestMorningNoonEveningRuleRoute(t *testing.T) {
	ans := multiJ(mealJ("breakfast", 0, foodJ("蛋餅", 300)), mealJ("lunch", 0, foodJ("便當", 700)), mealJ("dinner", 0, foodJ("滷肉飯", 600)))
	f, ic := newMealIntentFixture(t, answerJSON("none", "", "high"), ans)
	ic.failNow = true
	f.setNow(t, "2026-03-04 20:00")

	f.say(t, "早上吃了蛋餅，中午吃了便當，晚上吃了滷肉飯")

	if len(f.meals.Items) != 3 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	for i, want := range []string{"2026-03-04 08:00", "2026-03-04 12:30", "2026-03-04 18:30"} {
		if got := taipeiAt(&f.meals.Items[i]); got != want {
			t.Errorf("meal %d at %s, want %s", i, got, want)
		}
	}
	if ic.count() != 0 || len(f.qa.asked) != 0 {
		t.Errorf("intent calls = %d, qa calls = %d, want 0 and 0", ic.count(), len(f.qa.asked))
	}
}

// TestYesterdayAppliesToEveryMeal [F34-22] 開頭說一次「昨天」，兩餐都記在昨天（2026-03-03）.
func TestYesterdayAppliesToEveryMeal(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("breakfast", -1, foodJ("蛋餅", 300)), mealJ("dinner", -1, foodJ("拉麵", 600))))
	f.setNow(t, "2026-03-04 10:00")

	f.say(t, "昨天早餐吃了蛋餅，晚餐吃了拉麵")

	if len(f.meals.Items) != 2 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	for i, want := range []string{"2026-03-03 08:00", "2026-03-03 18:30"} {
		if got := taipeiAt(&f.meals.Items[i]); got != want {
			t.Errorf("meal %d at %s, want %s", i, got, want)
		}
	}
	mustContain(t, f.replyText(t), "昨天的早餐", "昨天的晚餐")
}

// TestDayBeforeYesterday [F34-22] 兩餐都記在前天（2026-03-02）.
func TestDayBeforeYesterday(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("lunch", -2, foodJ("雞腿便當", 750)), mealJ("dinner", -2, foodJ("火鍋", 900))))
	f.setNow(t, "2026-03-04 10:00")

	f.say(t, "前天午餐吃了雞腿便當、晚上吃了火鍋")

	if len(f.meals.Items) != 2 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	for i, want := range []string{"2026-03-02 12:30", "2026-03-02 18:30"} {
		if got := taipeiAt(&f.meals.Items[i]); got != want {
			t.Errorf("meal %d at %s, want %s", i, got, want)
		}
	}
	mustContain(t, f.replyText(t), "前天的午餐", "前天的晚餐")
}

// TestLateNightSeparateFromDinner [F34-22] 晚餐與宵夜是兩筆，升冪排列.
func TestLateNightSeparateFromDinner(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("late_night", 0, foodJ("鹹酥雞", 500)), mealJ("dinner", 0, foodJ("拉麵", 600))))
	f.setNow(t, "2026-03-04 23:30")

	f.say(t, "晚餐吃了拉麵，宵夜吃了鹹酥雞") // fake 故意把宵夜排在前面：存檔要依時間升冪

	if len(f.meals.Items) != 2 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	a, b := f.meals.Items[0], f.meals.Items[1]
	if a.MealType != "dinner" || taipeiAt(&a) != "2026-03-04 18:30" || b.MealType != "late_night" || taipeiAt(&b) != "2026-03-04 22:30" {
		t.Errorf("records = %s %s, %s %s", a.MealType, taipeiAt(&a), b.MealType, taipeiAt(&b))
	}
	mustContain(t, f.replyText(t), "宵夜")
}

// TestAfternoonIsSnack [F34-22] 「下午」記成點心（15:30）；餐別本身由真模型重播驗，這裡只驗程式把 snack 存對.
func TestAfternoonIsSnack(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", 0, foodJ("蛋糕", 350))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "下午吃了一塊蛋糕")

	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != "2026-03-04 15:30" {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	mustContain(t, reply, "已記錄點心（文字估算，拍照會更準）")
}

// TestMealLogPunctuationVariants [F34-22] 同一份三餐的四種寫法都被規則接住、各記三筆.
func TestMealLogPunctuationVariants(t *testing.T) {
	ans := multiJ(mealJ("breakfast", 0, foodJ("蛋餅", 300)), mealJ("lunch", 0, foodJ("便當", 700)), mealJ("dinner", 0, foodJ("牛肉麵", 650)))
	for name, text := range map[string]string{
		"semicolons":      "早餐：蛋餅；午餐：便當；晚餐：牛肉麵",
		"newlines":        "早餐蛋餅\n午餐便當\n晚餐牛肉麵",
		"crlf":            "早餐蛋餅\r\n午餐便當\r\n晚餐牛肉麵",
		"fullwidth space": "早餐：蛋餅　午餐：便當　晚餐：牛肉麵",
	} {
		t.Run(name, func(t *testing.T) {
			if !nutrition.LooksLikeMealLog(text) {
				t.Fatalf("LooksLikeMealLog(%q) = false", text)
			}
			f := newMealFixture(t, ans)
			f.setNow(t, "2026-03-04 20:00")
			f.say(t, text)
			if len(f.meals.Items) != 3 {
				t.Fatalf("meals = %+v", f.meals.Items)
			}
			for i, want := range []string{"2026-03-04 08:00", "2026-03-04 12:30", "2026-03-04 18:30"} {
				if got := taipeiAt(&f.meals.Items[i]); got != want {
					t.Errorf("meal %d at %s, want %s", i, got, want)
				}
			}
		})
	}
	if nutrition.LooksLikeMealLog("今天天氣真好") {
		t.Error("control: 今天天氣真好 is not a meal log")
	}
}

// TestResendWithinTenMinutesIsNotRecorded [F34-24] 10 分鐘內重送不重複記、不花估算；超過 10 分鐘照記；寫法不同但內容相同仍算重送，改一個字就不算.
func TestResendWithinTenMinutesIsNotRecorded(t *testing.T) {
	t.Run("+9m59s is a resend", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.setNow(t, "2026-03-04 20:09:59")

		f.say(t, screenshotText)

		if !strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了") {
			t.Errorf("reply: %s", f.replyText(t))
		}
		if len(f.meals.Items) != 3 || len(f.ai.reqs) != 1 {
			t.Errorf("meals = %d, estimate calls = %d, want 3 and 1", len(f.meals.Items), len(f.ai.reqs))
		}
		mustContain(t, f.replyText(t), "早餐、午餐、晚餐", "今日累計：1680 大卡（3 餐）")
	})
	t.Run("+10m01s records again", func(t *testing.T) {
		f := newMealFixture(t, shotJSON, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.setNow(t, "2026-03-04 20:10:01")

		f.say(t, screenshotText)

		if len(f.meals.Items) != 6 || len(f.ai.reqs) != 2 {
			t.Errorf("meals = %d, estimate calls = %d, want 6 and 2", len(f.meals.Items), len(f.ai.reqs))
		}
	})
	t.Run("a resend does not refresh the window", func(t *testing.T) {
		f := newMealFixture(t, shotJSON, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.setNow(t, "2026-03-04 20:08:00")
		f.say(t, screenshotText) // 重送，不刷新 At
		f.setNow(t, "2026-03-04 20:12:00")

		f.say(t, screenshotText) // 距第一次 12 分鐘：照記

		if len(f.meals.Items) != 6 {
			t.Errorf("meals = %d, want 6 (10 minutes count from the first record)", len(f.meals.Items))
		}
	})
	t.Run("same content in different spelling is a resend", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.setNow(t, "2026-03-04 20:02:00")

		f.say(t, "早餐吃　牛肉火腿起司三明治，午餐：黑麥酸種麵包兩個手掌大小，\r\n晚餐　福隆雞腿便當　加上哈根達斯草莓口味４匙　加上一杯牛奶！")

		if !strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了") || len(f.meals.Items) != 3 {
			t.Errorf("reply: %s meals = %d", f.replyText(t), len(f.meals.Items))
		}
	})
	t.Run("one changed character is not a resend", func(t *testing.T) {
		f := newMealFixture(t, shotJSON, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.setNow(t, "2026-03-04 20:02:00")

		f.say(t, strings.Replace(screenshotText, "4匙", "5匙", 1))

		if len(f.meals.Items) != 6 || len(f.ai.reqs) != 2 {
			t.Errorf("meals = %d, estimate calls = %d, want 6 and 2", len(f.meals.Items), len(f.ai.reqs))
		}
	})
	t.Run("a single meal is detected too", func(t *testing.T) {
		f := newMealFixture(t, noodleText)
		f.setNow(t, "2026-03-04 12:00:00")
		f.say(t, "午餐吃了牛肉麵")
		f.setNow(t, "2026-03-04 12:01:00")

		f.say(t, "午餐吃了牛肉麵")

		if !strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了（午餐）") || len(f.meals.Items) != 1 {
			t.Errorf("reply: %s meals = %d", f.replyText(t), len(f.meals.Items))
		}
	})
}

// softDeleteMeals DeleteByID 回報刪除成功，但讀取端暫時還看得到那筆（複本延遲）；模擬「紀錄還在、指標一定要清」的情境.
type softDeleteMeals struct{ *storagetest.Meals }

func (s *softDeleteMeals) DeleteByID(context.Context, string, string) (bool, error) { return true, nil }

// TestResendAfterCancelRecords [F34-24] 取消後重送要重新記（Forget 一併清掉指紋）.
func TestResendAfterCancelRecords(t *testing.T) {
	t.Run("cancel then resend", func(t *testing.T) {
		f := newMealFixture(t, shotJSON, shotJSON)
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.say(t, keywordCancelMeal)
		if len(f.meals.Items) != 0 {
			t.Fatalf("cancel left %d meals", len(f.meals.Items))
		}
		f.setNow(t, "2026-03-04 20:03:00")

		f.say(t, screenshotText)

		if len(f.meals.Items) != 3 || len(f.ai.reqs) != 2 {
			t.Errorf("meals = %d, estimate calls = %d, want 3 and 2; reply: %s", len(f.meals.Items), len(f.ai.reqs), f.replyText(t))
		}
	})
	t.Run("cancel clears the pointer even if the store still returns the rows", func(t *testing.T) {
		f := newMealFixture(t, shotJSON, shotJSON)
		f.h.mealRepo = &softDeleteMeals{Meals: f.meals}
		f.setNow(t, "2026-03-04 20:00:00")
		f.say(t, screenshotText)
		f.say(t, keywordCancelMeal)
		if _, ok := f.h.recent.Get(context.Background(), "U1"); ok {
			t.Fatal("cancel must forget the pointer (and its fingerprint)")
		}
		f.setNow(t, "2026-03-04 20:03:00")

		f.say(t, screenshotText)

		if strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了") || len(f.ai.reqs) != 2 {
			t.Errorf("a cancelled message must be recordable again: estimate calls = %d reply: %s", len(f.ai.reqs), f.replyText(t))
		}
	})
}

// TestResendAfterDeletedRecords [F34-24] 紀錄被別處（LIFF）刪光後重送要重新記.
func TestResendAfterDeletedRecords(t *testing.T) {
	f := newMealFixture(t, shotJSON, shotJSON)
	f.setNow(t, "2026-03-04 20:00:00")
	f.say(t, screenshotText)
	ids := make([]string, 0, 3)
	for _, m := range f.meals.Items {
		ids = append(ids, m.ID)
	}
	for _, id := range ids {
		if ok, err := f.meals.DeleteByID(context.Background(), "U1", id); err != nil || !ok {
			t.Fatalf("delete %s: %v %v", id, ok, err)
		}
	}
	f.setNow(t, "2026-03-04 20:03:00")

	f.say(t, screenshotText)

	if len(f.meals.Items) != 3 || strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了") {
		t.Errorf("meals = %d reply: %s", len(f.meals.Items), f.replyText(t))
	}
	// 只刪掉一部分：還有一筆在就仍算重送.
	f.setNow(t, "2026-03-04 20:04:00")
	if ok, err := f.meals.DeleteByID(context.Background(), "U1", f.meals.Items[0].ID); err != nil || !ok {
		t.Fatalf("delete one: %v %v", ok, err)
	}
	f.say(t, screenshotText)
	if !strings.HasPrefix(f.replyText(t), "剛剛已經記過這則了") || len(f.meals.Items) != 2 {
		t.Errorf("a partly deleted batch is still a resend: meals = %d reply: %s", len(f.meals.Items), f.replyText(t))
	}
}

// TestPlanSentencesAreNotRecorded [F34-20] 願望／計畫句不記：固定回覆、估算 AI 0 次、不計每日額度；完成句照記；混合句整則被擋（已知取捨）.
// 每個句子都試兩條路：規則直接接住，以及意圖分類器誤判成 log_meal/high（防護在 handleTextMeal，兩條路都要擋）.
func TestPlanSentencesAreNotRecorded(t *testing.T) {
	const want = "看起來還沒吃喔。吃完再傳給我，我再幫你記錄（例如「午餐吃了牛肉麵」）。"
	plans := []string{
		"明天中午想吃牛肉麵", "我想吃一碗牛肉麵", "晚餐打算吃火鍋", "如果中午吃便當",
		"中午吃了便當，晚餐想吃火鍋", // 混合句：整則擋，不只留中午（規格 #16）
	}
	f, _ := newMealIntentFixture(t, answerJSON("log_meal", "", "high"), noodleText)
	for _, text := range plans {
		f.say(t, text)
		if got := f.replyText(t); got != want {
			t.Errorf("%q → %q, want the fixed plan reply", text, got)
		}
	}
	if len(f.meals.Items) != 0 || len(f.ai.reqs) != 0 {
		t.Fatalf("plans must not record or estimate: meals=%d estimates=%d", len(f.meals.Items), len(f.ai.reqs))
	}
	// 每日額度：60 則計畫句都不計次，之後的真記餐照常估算（若計次，這則會收到「次數已達上限」）.
	for range textMealDailyLimit {
		f.say(t, plans[0])
	}
	f.say(t, "午餐吃了牛肉麵")
	if len(f.meals.Items) != 1 || len(f.ai.reqs) != 1 {
		t.Errorf("after %d plan sentences a real meal is still estimated: meals=%d estimates=%d reply=%s",
			textMealDailyLimit, len(f.meals.Items), len(f.ai.reqs), f.replyText(t))
	}

	// 正對照：過去式不誤擋.
	for _, text := range []string{"昨天晚餐吃了拉麵", "午餐吃了牛肉麵"} {
		g := newMealFixture(t, noodleText)
		g.say(t, text)
		if len(g.meals.Items) != 1 {
			t.Errorf("%q must still be recorded", text)
		}
	}

	// 「再加」路徑（addToRecentMeal）同樣擋計畫句：不呼叫估算、不改 recent 那一筆.
	h := newMealFixture(t, noodleText, eggText)
	h.say(t, "午餐吃了牛肉麵")
	before := h.meals.Items[0]
	h.say(t, "再加明天的便當")
	if got := h.replyText(t); got != want {
		t.Errorf("再加 plan reply = %q, want the fixed plan reply", got)
	}
	if len(h.ai.reqs) != 1 || len(h.meals.Items) != 1 || len(h.meals.Items[0].Items) != len(before.Items) || h.meals.Items[0].Total != before.Total {
		t.Errorf("the recent meal must stay untouched: reqs=%d meals=%+v", len(h.ai.reqs), h.meals.Items)
	}
}

const bentoClarifyDinner = `{"is_food":true,"meal_type":"dinner","clarify":{"question":"便當差很多，是哪一種？","options":["雞腿便當","排骨便當"]},"meals":[]}`

// TestClarifyThenTwoMeals [F34-27] 追問後重估出兩餐：各餐用 AI 回的餐別，追問前說過的餐別不蓋任何一餐.
func TestClarifyThenTwoMeals(t *testing.T) {
	t.Run("explicit types from the model", func(t *testing.T) {
		f := newMealFixture(t, `{"is_food":true,"meal_type":"","clarify":{"question":"便當是哪一種？","options":["雞腿便當","排骨便當"]},"meals":[]}`,
			multiJ(mealJ("breakfast", 0, foodJ("雞腿便當", 750)), mealJ("lunch", 0, foodJ("牛肉麵", 650))))
		f.setNow(t, "2026-03-04 20:00")

		f.say(t, "早餐吃便當，午餐吃了牛肉麵")
		if len(f.meals.Items) != 0 {
			t.Fatal("nothing is saved before the answer")
		}
		if st, err := f.h.convManager.Get(context.Background(), "U1"); err != nil || st == nil || st.Meal == nil {
			t.Fatalf("a pending state must exist: %+v %v", st, err)
		}
		f.say(t, "雞腿便當")

		if len(f.meals.Items) != 2 || f.meals.Items[0].MealType != "breakfast" || f.meals.Items[1].MealType != "lunch" {
			t.Errorf("meals = %+v", f.meals.Items)
		}
	})
	t.Run("pending type must not overwrite an untyped meal of several", func(t *testing.T) {
		// 追問前頂層餐別是 dinner；重估的第一餐沒給餐別 → 依時間推（8:00 = 早餐），不是被 dinner 蓋掉.
		f := newMealFixture(t, bentoClarifyDinner,
			multiJ(mealJ("", 0, foodJ("雞腿便當", 750)), mealJ("lunch", 0, foodJ("牛肉麵", 650))))
		f.setNow(t, "2026-03-04 08:00")

		f.say(t, "早餐吃便當，午餐吃了牛肉麵")
		f.say(t, "雞腿便當")

		if len(f.meals.Items) != 2 || f.meals.Items[0].MealType != "breakfast" || f.meals.Items[1].MealType != "lunch" {
			t.Errorf("meals = %+v", f.meals.Items)
		}
	})
	t.Run("a single meal still takes the pending type", func(t *testing.T) {
		f := newMealFixture(t, bentoClarifyDinner, porkBentoText)
		f.setNow(t, "2026-03-04 08:00")

		f.say(t, "晚餐吃便當")
		f.say(t, "排骨便當")

		if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "dinner" {
			t.Errorf("meals = %+v", f.meals.Items)
		}
	})
}

// TestClarifyKeepsWholeLongText [F34-27] 追問的原文不截成 100 字：最後一餐的關鍵字還在狀態與第二次估算的 prompt 裡.
func TestClarifyKeepsWholeLongText(t *testing.T) {
	seg := strings.Repeat("蛋餅豆漿", 12)
	long := "早餐吃了" + seg + "\n午餐吃了" + seg + "\n晚餐吃了" + seg + "酸辣湯"
	if n := len([]rune(long)); n < 150 || n > 500 {
		t.Fatalf("fixture text is %d runes", n)
	}
	f := newMealFixture(t, bentoClarifyDinner, multiJ(mealJ("breakfast", 0, foodJ("蛋餅", 300)), mealJ("dinner", 0, foodJ("酸辣湯", 100))))

	f.say(t, long)
	st, err := f.h.convManager.Get(context.Background(), "U1")
	if err != nil || st == nil || st.Meal == nil || !strings.Contains(st.Meal.Text, "酸辣湯") {
		t.Fatalf("pending text must keep the last meal's keyword: %+v %v", st, err)
	}
	f.say(t, "雞腿便當")

	if !strings.Contains(f.ai.last().Prompt, "酸辣湯") {
		t.Error("the second estimate's prompt lost the last meal")
	}
	if len(f.meals.Items) != 2 || len([]rune(f.meals.Items[0].Note)) > maxNoteRunes {
		t.Errorf("the saved Note stays within %d runes: %+v", maxNoteRunes, f.meals.Items)
	}
}

// TestUntypedMealIsInferredThenMerged [F34-21] 沒餐別的餐先依時間推、推完再與同餐合併：14:00 兩筆（點心＋晚餐），19:00 一筆晚餐含 A+B.
func TestUntypedMealIsInferredThenMerged(t *testing.T) {
	ans := multiJ(mealJ("", 0, foodJ("蛋糕", 350)), mealJ("dinner", 0, foodJ("拉麵", 600)))
	t.Run("14:00", func(t *testing.T) {
		f := newMealFixture(t, ans)
		f.setNow(t, "2026-03-04 14:00")
		f.say(t, "吃了蛋糕，晚餐吃了拉麵")
		if len(f.meals.Items) != 2 || f.meals.Items[0].MealType != "snack" || f.meals.Items[1].MealType != "dinner" {
			t.Errorf("meals = %+v", f.meals.Items)
		}
	})
	t.Run("19:00", func(t *testing.T) {
		f := newMealFixture(t, ans)
		f.setNow(t, "2026-03-04 19:00")
		f.say(t, "吃了蛋糕，晚餐吃了拉麵")
		if len(f.meals.Items) != 1 {
			t.Fatalf("two dinners must be one record: %+v", f.meals.Items)
		}
		m := f.meals.Items[0]
		if m.MealType != "dinner" || len(m.Items) != 2 || m.Items[0].Name != "蛋糕" || m.Items[1].Name != "拉麵" || m.Total.Calories != 950 {
			t.Errorf("merged = %+v", m)
		}
	})
}

// threeMealEstimate 三餐的估算結果（直接餵 saveTextMeals，不經 AI）.
func threeMealEstimate() *nutrition.TextEstimate {
	one := func(typ nutrition.MealType) nutrition.MealEstimate {
		return nutrition.MealEstimate{MealType: typ, Foods: guardFoods, Total: guardFoods[0].Nutrients}
	}
	return &nutrition.TextEstimate{IsFood: true, Meals: []nutrition.MealEstimate{one(nutrition.Breakfast), one(nutrition.Lunch), one(nutrition.Dinner)}}
}

// TestMultiMealSaveIsUndoneAfterErasure [F34-22] 多餐寫入與刪除資料的競態（規格 #21）：
// (a) 寫前標記 → 0 筆；(b) 第 2 筆寫入之後才出現標記 → 3 筆全補刪、recent 清掉；(c) (b) 且補刪回錯 → 不說「已經刪除」；標記早於訊息 → 全留.
func TestMultiMealSaveIsUndoneAfterErasure(t *testing.T) {
	save := func(e *guardEnv, ctx context.Context) {
		e.h.saveTextMeals(ctx, guardUID, "tok", "早餐午餐晚餐吃了牛肉麵", threeMealEstimate())
	}
	t.Run("a_marked_before_first_insert", func(t *testing.T) {
		e := newGuardEnv(t)
		markErased(t, e.h.erased, context.Background(), guardUID)

		save(e, sentMinuteAgo())

		assertReply(t, e.api.lastReply(t), []string{erasedMessage}, []string{"已記錄"})
		if len(e.meals.Items) != 0 {
			t.Errorf("nothing may be written: %+v", e.meals.Items)
		}
	})
	t.Run("b_marked_after_second_insert", func(t *testing.T) {
		e := newGuardEnv(t)
		inserts := 0
		e.beforeWrite = func(kind string) {
			if kind != "meal" {
				return
			}
			if inserts++; inserts == 2 {
				markErased(t, e.h.erased, context.Background(), guardUID)
			}
		}

		save(e, sentMinuteAgo())

		assertReply(t, e.api.lastReply(t), []string{erasedMessage}, []string{"已記錄", "今日累計"})
		if len(e.meals.Items) != 0 {
			t.Errorf("all three meals must be removed, left: %+v", e.meals.Items)
		}
		if e.undoCalls["meal"] != 3 || e.undoCalls["recent"] != 1 {
			t.Errorf("undo calls = %v, want three DeleteByID and one Forget", e.undoCalls)
		}
		if _, ok := e.h.recent.Get(context.Background(), guardUID); ok {
			t.Error("recent must be forgotten")
		}
	})
	t.Run("c_undo_fails", func(t *testing.T) {
		e := newGuardEnv(t)
		inserts := 0
		e.beforeWrite = func(kind string) {
			if kind == "meal" {
				if inserts++; inserts == 2 {
					markErased(t, e.h.erased, context.Background(), guardUID)
				}
			}
		}
		e.failUndo["meal"] = true

		save(e, sentMinuteAgo())

		assertReply(t, e.api.lastReply(t), []string{eraseFailedMessage}, []string{erasedMessage, "已記錄"})
	})
	t.Run("marker_before_message_keeps_everything", func(t *testing.T) {
		e := newGuardEnv(t)
		markErased(t, e.h.erased, context.Background(), guardUID)

		save(e, futureSent())

		assertReply(t, e.api.lastReply(t), []string{"已記錄", "共 3 餐"}, []string{erasedMessage})
		if len(e.meals.Items) != 3 {
			t.Errorf("meals = %d, want 3", len(e.meals.Items))
		}
		if got, ok := e.h.recent.Get(context.Background(), guardUID); !ok || len(got.IDs) != 3 {
			t.Errorf("recent = %+v ok=%v, want a batch of 3", got, ok)
		}
	})
}

// insertFailingMeals 第 failAt 次 Insert 回錯（與正式 store 一樣：ID 在寫入前已設好，資料不落地）.
type insertFailingMeals struct {
	*storagetest.Meals
	calls  int
	failAt int // 0 = 全部失敗
}

func (s *insertFailingMeals) Insert(ctx context.Context, m *meal.Meal) error {
	s.calls++
	if s.failAt == 0 || s.calls == s.failAt {
		m.ID = fmt.Sprintf("failed-%d", s.calls)
		return errors.New("insert failed org_secret123456")
	}
	return s.Meals.Insert(ctx, m)
}

// TestMultiMealPartialInsertFailure [F34-22] 第 2 筆寫入失敗：另 2 筆存成、失敗那餐標記、recent 只含成功的；全失敗則不記 recent.
func TestMultiMealPartialInsertFailure(t *testing.T) {
	t.Run("second insert fails", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.setNow(t, "2026-03-04 20:00")
		repo := &insertFailingMeals{Meals: f.meals, failAt: 2}
		f.h.mealRepo = repo

		f.say(t, screenshotText)

		if len(f.meals.Items) != 2 || f.meals.Items[0].MealType != "breakfast" || f.meals.Items[1].MealType != "dinner" {
			t.Fatalf("the other two must be saved: %+v", f.meals.Items)
		}
		text := f.replyText(t)
		if strings.Count(text, failedSaveNote) != 1 {
			t.Fatalf("exactly one block carries the failure note: %s", text)
		}
		lunchAt, noteAt, dinnerAt := strings.Index(text, "【午餐】"), strings.Index(text, failedSaveNote), strings.Index(text, "【晚餐】")
		if !(lunchAt >= 0 && lunchAt < noteAt && noteAt < dinnerAt) {
			t.Errorf("the note belongs at the end of the lunch block: %s", text)
		}
		if strings.Contains(text, "org_secret") {
			t.Error("the store error must not leak into the reply")
		}
		recent, ok := f.h.recent.Get(context.Background(), "U1")
		if !ok || len(recent.IDs) != 2 || recent.IDs[0] != f.meals.Items[0].ID || recent.IDs[1] != f.meals.Items[1].ID {
			t.Errorf("recent = %+v, want exactly the two saved IDs", recent)
		}
	})
	t.Run("every insert fails", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.h.mealRepo = &insertFailingMeals{Meals: f.meals}

		f.say(t, screenshotText)

		if !strings.HasSuffix(f.replyText(t), failedSaveNote) || len(f.meals.Items) != 0 {
			t.Errorf("reply: %s meals=%d", f.replyText(t), len(f.meals.Items))
		}
		if _, ok := f.h.recent.Get(context.Background(), "U1"); ok {
			t.Error("nothing saved, nothing remembered")
		}
	})
}

// TestBatchRecentOperations [F34-23] 一次記了多餐之後：份量、再加、店家回固定句且不改任何一筆；照片當新的一餐；取消整批刪.
func TestBatchRecentOperations(t *testing.T) {
	const batchMsg = "剛剛一次記了多餐，份量、店家請到「我的統計」逐筆修改，或輸入「取消這筆」整批取消"
	totals := func(f *mealFixture) []float64 {
		var out []float64
		for _, m := range f.meals.Items {
			out = append(out, m.Total.Calories)
		}
		return out
	}
	t.Run("edits are refused, photo is a new meal", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.setNow(t, "2026-03-04 20:00")
		f.say(t, screenshotText)
		before := fmt.Sprint(totals(f))
		estimates := len(f.ai.reqs)

		for _, cmd := range []string{keywordHalf, "再加一顆滷蛋", keywordMealPlace} {
			f.say(t, cmd)
			if got := f.replyText(t); got != batchMsg {
				t.Errorf("%q → %q, want the fixed batch message", cmd, got)
			}
		}
		if fmt.Sprint(totals(f)) != before || len(f.ai.reqs) != estimates {
			t.Errorf("no record may change and no estimate may run: totals %v (was %s), estimates %d", totals(f), before, len(f.ai.reqs))
		}

		f.photo(t, noodlePhoto)
		if len(f.meals.Items) != 4 {
			t.Fatalf("the photo is a new meal: %d records", len(f.meals.Items))
		}
		if f.meals.Items[3].Source == meal.SourceText || f.meals.Items[2].Total.Calories != 1130 {
			t.Errorf("the dinner record must not absorb the photo: %+v", f.meals.Items)
		}
	})
	t.Run("cancel deletes the whole batch", func(t *testing.T) {
		f := newMealFixture(t, shotJSON)
		f.setNow(t, "2026-03-04 20:00")
		f.say(t, screenshotText)

		f.say(t, keywordCancelMeal)

		mustContain(t, f.replyText(t), "已取消這批紀錄（共 3 筆）")
		if len(f.meals.Items) != 0 {
			t.Errorf("left: %+v", f.meals.Items)
		}
		f.say(t, keywordCancelMeal)
		mustContain(t, f.replyText(t), "找不到 30 分鐘內記錄的餐點")
	})
	t.Run("single cancel text is unchanged", func(t *testing.T) {
		f := newMealFixture(t, noodleText)
		f.say(t, "午餐吃了牛肉麵")
		f.say(t, keywordCancelMeal)
		if !strings.HasPrefix(f.replyText(t), "已取消這筆紀錄") {
			t.Errorf("reply: %s", f.replyText(t))
		}
	})
	t.Run("batch cancel with a failing delete keeps the pointer", func(t *testing.T) {
		e := newGuardEnv(t)
		e.ai.texts = []string{shotJSON}
		e.h.handleTextMessage(context.Background(), guardUID, screenshotText, "tok")
		if len(e.meals.Items) != 3 {
			t.Fatalf("meals = %d", len(e.meals.Items))
		}
		e.failUndo["meal"] = true // 這個 fake 的 DeleteByID 會回錯
		e.h.handleTextMessage(context.Background(), guardUID, keywordCancelMeal, "tok")
		mustContain(t, e.api.lastReply(t), "刪除失敗")
		if _, ok := e.h.recent.Get(context.Background(), guardUID); !ok {
			t.Error("a failed batch cancel must not forget the pointer")
		}
	})
}

// TestFiveMealsReplyFitsLine [F34-22] 5 餐每餐 12 品項：回覆不超過 LINE 上限；太長時改精簡格式仍列出 5 個標籤與今日累計.
func TestFiveMealsReplyFitsLine(t *testing.T) {
	build := func(nameOf func(meal, food int) string) string {
		// 品名越長、份量描述越長，每餐越接近 LINE 上限；份量上限 20 字（maxPortionRunes）.
		portion := strings.Repeat("份", 20)
		types := []string{"breakfast", "lunch", "snack", "dinner", "late_night"}
		var meals []string
		for i, typ := range types {
			var foods []string
			for j := range 12 {
				foods = append(foods, fmt.Sprintf(`{"name":%q,"portion":%q,"calories":100}`, nameOf(i, j), portion))
			}
			meals = append(meals, mealJ(typ, 0, foods...))
		}
		return multiJ(meals...)
	}
	labels := []string{"早餐", "午餐", "點心", "晚餐", "宵夜"}
	t.Run("short names keep the full format", func(t *testing.T) {
		f := newMealFixture(t, build(func(i, j int) string { return fmt.Sprintf("菜%c%c", rune('甲'+i), rune('A'+j)) }))
		f.setNow(t, "2026-03-04 23:59")
		f.say(t, "早餐午餐點心晚餐宵夜都吃了很多")
		text := f.replyText(t)
		mustContain(t, text, "共 5 餐", "今日累計：6000 大卡（5 餐）", "• ")
		for _, l := range labels {
			mustContain(t, text, "【"+l+"】")
		}
	})
	t.Run("long names switch to the compact format", func(t *testing.T) {
		long := strings.Repeat("超", 26)
		f := newMealFixture(t, build(func(i, j int) string { return fmt.Sprintf("%s%c%c", long, rune('甲'+i), rune('A'+j)) }))
		f.setNow(t, "2026-03-04 23:59")
		f.say(t, "早餐午餐點心晚餐宵夜都吃了很多")
		text := f.replyText(t) // f.say 已經過 assertLINELimits（≤5000 字）
		mustContain(t, text, "共 5 餐", "今日累計：6000 大卡（5 餐）", "合計：1200 大卡")
		if strings.Contains(text, "• ") {
			t.Error("compact format lists no per-food lines")
		}
		for _, l := range labels {
			mustContain(t, text, "【"+l+"】")
		}
	})
	t.Run("more than five meals say so on the last line", func(t *testing.T) {
		var meals []string
		for _, typ := range []string{"breakfast", "lunch", "snack", "dinner", "late_night"} {
			meals = append(meals, mealJ(typ, 0, foodJ("菜"+typ[:1], 100)))
		}
		meals = append(meals, mealJ("dinner", -1, foodJ("昨晚的菜", 100)))
		f := newMealFixture(t, multiJ(meals...))
		f.setNow(t, "2026-03-04 23:59")
		f.say(t, "早餐午餐點心晚餐宵夜昨天晚餐都吃了")
		lines := strings.Split(f.replyText(t), "\n")
		if len(f.meals.Items) != 5 || lines[len(lines)-1] != "（一次最多記 5 餐，後面的請另外再傳一次）" {
			t.Errorf("meals = %d last line = %q", len(f.meals.Items), lines[len(lines)-1])
		}
	})
}

// TestMultiMealCountsOnceTowardDailyLimit [F34-22] 一則訊息只算 1 次估算額度（不是按餐數）；重送與計畫句不計.
func TestMultiMealCountsOnceTowardDailyLimit(t *testing.T) {
	texts := []string{shotJSON}
	for range textMealDailyLimit - 1 {
		texts = append(texts, noodleText)
	}
	f := newMealFixture(t, texts...)
	f.setNow(t, "2026-03-04 20:00:00")

	f.say(t, screenshotText)
	f.say(t, screenshotText) // 重送：不計
	f.say(t, "我想吃一碗牛肉麵")     // 計畫句：不計
	for i := range textMealDailyLimit - 1 {
		f.say(t, fmt.Sprintf("午餐吃了牛肉麵第%d號", i))
	}
	if len(f.ai.reqs) != textMealDailyLimit {
		t.Fatalf("estimate calls = %d, want %d (the 3-meal message counted once)", len(f.ai.reqs), textMealDailyLimit)
	}
	f.say(t, "午餐吃了牛肉麵最後一碗")

	if got := f.replyText(t); got != "今天的飲食估算次數已達上限，明天再來喔！" {
		t.Errorf("the 61st estimate must be refused, got %q", got)
	}
	if len(f.ai.reqs) != textMealDailyLimit {
		t.Errorf("estimate calls = %d after the refusal", len(f.ai.reqs))
	}
}

// TestScreenshotTextPassesAllGuards [F34-26] 截圖原文不會被計畫防護、問句判斷或改紀錄閘門擋掉.
func TestScreenshotTextPassesAllGuards(t *testing.T) {
	if nutrition.LooksLikePlan(screenshotText) {
		t.Error("LooksLikePlan must be false (「三明治」不含「明天」)")
	}
	if nutrition.LooksLikeQuestion(screenshotText) {
		t.Error("LooksLikeQuestion must be false")
	}
	if EditGate(screenshotText) {
		t.Error("EditGate must be false: the record-edit planner must not take a meal log")
	}
	if !nutrition.LooksLikeMealLog(screenshotText) {
		t.Error("the rule must accept the screenshot text")
	}
}
