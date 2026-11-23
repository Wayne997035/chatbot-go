package webhook

import (
	"chatbot-go/internal/conversation"
	"context"
	"strings"
	"testing"
)

// [F35-01][F35-03] 零食類名稱依該筆的 EatenAt（台灣 14:00–17:59 叫下午茶，其他叫點心）、點心記錄時間依原文有沒有「下午」.
// 預期值取自決策 9237522a / 035854b2 / d1138994 / abbf6591，不取自實作輸出；時鐘一律用 f.setNow 釘住.

// singleSnack 斷言只存了一筆 snack、用餐時間是 wantAt（台灣時間）.
func singleSnack(t *testing.T, f *mealFixture, wantAt string) {
	t.Helper()
	if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != "snack" || taipeiAt(&f.meals.Items[0]) != wantAt {
		t.Fatalf("meals = %+v, want one snack at %s", f.meals.Items, wantAt)
	}
}

// containsOnly 回覆含 want、不含 other.
func containsOnly(t *testing.T, reply, want, other string) {
	t.Helper()
	mustContain(t, reply, want)
	if strings.Contains(reply, other) {
		t.Errorf("reply must not contain %q: %s", other, reply)
	}
}

// W1：沒說餐別的飲料（snack_only），名稱跟著記錄時間（現在）走，邊界逐點驗.
func TestSnackReplyLabelFollowsEatenAt(t *testing.T) {
	cases := []struct {
		now, want, other string
	}{
		{"2026-03-04 13:59", "已記錄點心（", "下午茶"},
		{"2026-03-04 14:00", "已記錄下午茶（", "點心"},
		{"2026-03-04 17:59", "已記錄下午茶（", "點心"},
		{"2026-03-04 18:00", "已記錄點心（", "下午茶"},
		{"2026-03-04 10:30", "已記錄點心（", "下午茶"},
		{"2026-03-04 20:00", "已記錄點心（", "下午茶"},
	}
	for _, c := range cases {
		t.Run(c.now, func(t *testing.T) {
			f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
			f.setNow(t, c.now)

			reply := f.say(t, drinkText)

			singleSnack(t, f, c.now)
			containsOnly(t, reply, c.want, c.other)
		})
	}
}

// W3：昨天下午茶 → 昨天 15:30，名稱用該筆的時間（不是現在的 20:00）：「昨天的下午茶」.
func TestYesterdayTeaSnackIsTeaTime(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", -1, foodJ("蛋糕", 350))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "昨天下午茶吃了蛋糕")

	singleSnack(t, f, "2026-03-03 15:30")
	containsOnly(t, reply, "已記錄昨天的下午茶", "點心")
}

// W6 / S9 共用：照片路徑直接叫 recordPhotoMeal.
func TestPhotoSnackLabelFollowsEatenAt(t *testing.T) {
	cases := []struct {
		name, now string
		pending   *conversation.PendingMeal
		want      string
	}{
		{"snack only at 15:00", "2026-03-04 15:00", nil, "已記錄下午茶"},
		{"snack only at 20:00", "2026-03-04 20:00", nil, "已記錄點心"},
		{"pending yesterday tea", "2026-03-04 20:00",
			&conversation.PendingMeal{MealType: "snack", DayOffset: -1, Text: "昨天下午茶喝了珍奶"}, "已記錄昨天的下午茶"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMealFixture(t)
			f.setNow(t, c.now)

			f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(true), c.pending)

			mustContain(t, f.replyText(t), c.want)
		})
	}
}

// W7：10 分鐘內重送同一則，回覆的名稱用那筆 15:00 的時間 →「（下午茶）」.
func TestResendReplyUsesTeaLabel(t *testing.T) {
	f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
	f.setNow(t, "2026-03-04 15:00:00")
	f.say(t, drinkText)
	f.setNow(t, "2026-03-04 15:01:00")

	f.say(t, drinkText)

	if reply := f.replyText(t); !strings.HasPrefix(reply, "剛剛已經記過這則了（下午茶）") || len(f.meals.Items) != 1 {
		t.Errorf("reply: %s meals = %d", reply, len(f.meals.Items))
	}
}

// W8：再加一個、用照片更新，都沿用剛記的那筆（15:00）的名稱.
func TestAddToAndPhotoMergeUseTeaLabel(t *testing.T) {
	t.Run("add to the latest snack", func(t *testing.T) {
		f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))), eggText)
		f.setNow(t, "2026-03-04 15:00")
		f.say(t, drinkText)

		reply := f.say(t, "再加一個滷蛋")

		containsOnly(t, reply, "➕ 已加到剛剛的下午茶", "點心")
	})
	t.Run("photo updates the latest snack", func(t *testing.T) {
		f := newMealFixture(t, multiJ(snackMealJ("", 0, "true", foodJ("珍珠奶茶", 440))))
		f.setNow(t, "2026-03-04 15:00")
		f.say(t, drinkText)

		reply := f.photo(t, noodlePhoto)

		containsOnly(t, reply, "📷 已用照片更新剛剛的下午茶", "點心")
	})
}

// S1：20:00 說「點心吃了雞排」→ 記在現在 20:00，叫點心（不是 15:30 / 下午茶）.
func TestStatedPlainSnackIsRecordedAtNow(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", 0, foodJ("雞排", 500))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "點心吃了雞排")

	singleSnack(t, f, "2026-03-04 20:00")
	containsOnly(t, reply, "已記錄點心（文字估算，拍照會更準）", "下午茶")
}

// S2：16:30 說「點心吃了蛋糕」→ 記在現在 16:30（不是 15:30），名稱依時間叫下午茶.
func TestStatedPlainSnackAtTeaHourKeepsNow(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", 0, foodJ("蛋糕", 350))))
	f.setNow(t, "2026-03-04 16:30")

	reply := f.say(t, "點心吃了蛋糕")

	singleSnack(t, f, "2026-03-04 16:30")
	mustContain(t, reply, "已記錄下午茶（")
}

// S3：昨天說點心 → 昨天同一時刻 20:00（不是昨天 15:30）.
func TestYesterdayPlainSnackKeepsClock(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", -1, foodJ("雞排", 500))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "昨天點心吃了雞排")

	singleSnack(t, f, "2026-03-03 20:00")
	containsOnly(t, reply, "已記錄昨天的點心", "下午茶")
}

// S4：20:00 說「下午茶吃了蛋糕」→ 15:30，叫下午茶（d1138994 取代 035854b2 的「晚上說下午茶顯示點心」）.
func TestStatedTeaSnackKeepsTypicalTime(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", 0, foodJ("蛋糕", 350))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "下午茶吃了蛋糕")

	singleSnack(t, f, "2026-03-04 15:30")
	containsOnly(t, reply, "已記錄下午茶", "點心")
}

// S5：整句判斷的已知近似：句子裡有「下午」，同句的點心也放 15:30；午餐 12:30 不動.
func TestMixedSentenceWithAfternoonKeepsSnackAt1530(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("lunch", 0, foodJ("便當", 700)), mealJ("snack", 0, foodJ("雞排", 500))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "下午吃了便當，點心吃了雞排")

	if len(f.meals.Items) != 2 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	for i := range f.meals.Items {
		m := &f.meals.Items[i]
		switch m.MealType {
		case "lunch":
			if got := taipeiAt(m); got != "2026-03-04 12:30" {
				t.Errorf("lunch at %s, want 12:30", got)
			}
		case "snack":
			if got := taipeiAt(m); got != "2026-03-04 15:30" {
				t.Errorf("snack at %s, want 15:30 (whole-sentence rule)", got)
			}
		default:
			t.Errorf("unexpected meal type %q", m.MealType)
		}
	}
	mustContain(t, reply, "【下午茶】", "【午餐】")
}

// S6：原文沒有「下午」時只有點心挪到現在，午餐不動（守住 MealType == Snack 的條件）.
func TestPlainSnackWordDoesNotMoveOtherMeals(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("lunch", 0, foodJ("便當", 700)), mealJ("snack", 0, foodJ("雞排", 500))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "午餐吃了便當，點心吃了雞排")

	if len(f.meals.Items) != 2 {
		t.Fatalf("meals = %+v", f.meals.Items)
	}
	for i := range f.meals.Items {
		m := &f.meals.Items[i]
		switch m.MealType {
		case "lunch":
			if got := taipeiAt(m); got != "2026-03-04 12:30" {
				t.Errorf("lunch at %s, want 12:30 (must not move)", got)
			}
		case "snack":
			if got := taipeiAt(m); got != "2026-03-04 20:00" {
				t.Errorf("snack at %s, want 20:00", got)
			}
		default:
			t.Errorf("unexpected meal type %q", m.MealType)
		}
	}
	mustContain(t, reply, "【午餐】", "【點心】")
}

// S7：模型判成點心、使用者兩個詞都沒說（零食吃了洋芋片）→ 記在現在，叫點心.
func TestSnackWithoutAfternoonWordIsRecordedAtNow(t *testing.T) {
	f := newMealFixture(t, multiJ(mealJ("snack", 0, foodJ("洋芋片", 300))))
	f.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, "零食吃了洋芋片")

	singleSnack(t, f, "2026-03-04 20:00")
	containsOnly(t, reply, "已記錄點心（", "下午茶")
}

// S8：追問路徑：判斷「下午」要看第一次的原文，不是只看追問的答案.
func TestClarifyPathKeepsStatedSnackWord(t *testing.T) {
	clarify := `{"is_food":true,"meal_type":"snack","clarify":{"question":"飲料差很多，是哪一種？","options":["珍珠奶茶","紅茶"]},"meals":[]}`
	answer := multiJ(mealJ("snack", 0, foodJ("珍珠奶茶", 440)))
	t.Run("點心 stays at now", func(t *testing.T) {
		f := newMealFixture(t, clarify, answer)
		f.setNow(t, "2026-03-04 20:00")
		f.say(t, "點心喝了飲料")
		if len(f.meals.Items) != 0 {
			t.Fatal("nothing is saved before the answer")
		}

		reply := f.say(t, "珍珠奶茶")

		singleSnack(t, f, "2026-03-04 20:00")
		containsOnly(t, reply, "已記錄點心", "下午茶")
	})
	t.Run("下午茶 keeps 15:30 though the answer has no 下午", func(t *testing.T) {
		f := newMealFixture(t, clarify, answer)
		f.setNow(t, "2026-03-04 20:00")
		f.say(t, "下午茶喝了飲料")

		reply := f.say(t, "珍珠奶茶")

		singleSnack(t, f, "2026-03-04 15:30")
		containsOnly(t, reply, "已記錄下午茶", "點心")
	})
}

// S9：照片有追問描述（pending）時，描述有沒有「下午」決定記錄時間；沒有 pending 的照片不受影響.
func TestPhotoPendingSnackWordDecidesTime(t *testing.T) {
	cases := []struct {
		name      string
		pending   *conversation.PendingMeal
		wantAt    string
		wantType  string
		wantReply string
	}{
		{"點心 at now", &conversation.PendingMeal{MealType: "snack", Text: "點心喝珍奶"}, "2026-03-04 20:00", "snack", "已記錄點心"},
		{"下午茶 at 15:30", &conversation.PendingMeal{MealType: "snack", Text: "下午茶喝珍奶"}, "2026-03-04 15:30", "snack", "已記錄下午茶"},
		{"yesterday 點心 keeps the clock", &conversation.PendingMeal{MealType: "snack", DayOffset: -1, Text: "昨天點心吃的"},
			"2026-03-03 20:00", "snack", "已記錄昨天的點心"},
		{"yesterday 下午茶 at 15:30", &conversation.PendingMeal{MealType: "snack", DayOffset: -1, Text: "昨天下午茶"},
			"2026-03-03 15:30", "snack", "已記錄昨天的下午茶"},
		{"neither word at now", &conversation.PendingMeal{MealType: "snack", Text: "喝珍奶"}, "2026-03-04 20:00", "snack", "已記錄點心"},
		{"lunch never moves", &conversation.PendingMeal{MealType: "lunch", Text: "點心吃了便當"}, "2026-03-04 12:30", "lunch", "已記錄午餐"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMealFixture(t)
			f.setNow(t, "2026-03-04 20:00")

			f.h.recordPhotoMeal(context.Background(), "U1", "tok", photoSnackAnalysis(true), c.pending)

			if len(f.meals.Items) != 1 || f.meals.Items[0].MealType != c.wantType || taipeiAt(&f.meals.Items[0]) != c.wantAt {
				t.Fatalf("meals = %+v, want one %s at %s", f.meals.Items, c.wantType, c.wantAt)
			}
			mustContain(t, f.replyText(t), c.wantReply)
		})
	}
}

// 下面三支走完整的改餐別流程（確認卡 + 按確認後的成功訊息），名稱依各筆自己的時間.
const (
	planToBreakfastInPlace = `{"op":"set_meal_type","target":"M1","meal_type":"breakfast"}`
	planToSnackInPlace     = `{"op":"set_meal_type","target":"M1","meal_type":"snack"}`
	planTailItemsToSnack   = `{"op":"move_items","target":"M1","moves":[{"items":[4,5],"meal_type":"snack"}]}`
)

// R3：原地改成點心，新名稱依新的 EatenAt：20:00 提議 → 15:30 → 下午茶；成功訊息的「（原為晚餐）」不變.
func TestRegroupInPlaceSnackLabelFollowsNewEatenAt(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planToSnackInPlace)
	mustContain(t, replyText(t, f), "・晚餐：改為下午茶（1810 大卡不變）")

	_, reply := f.tapWith(t, ok, nil)

	mustContain(t, reply, "・下午茶 1810 大卡（原為晚餐）")
	if m := f.mealByID(src.ID); m == nil || !m.EatenAt.Equal(rgToday(15, 30)) {
		t.Errorf("record = %+v, want eaten at 15:30", m)
	}
}

// R4：併入既有的點心，名稱依目標那筆的時間（15:00 → 下午茶、20:00 → 點心），確認卡與成功訊息一致.
func TestRegroupMergeLabelFollowsTargetEatenAt(t *testing.T) {
	for _, c := range []struct {
		name         string
		hour         int
		card, result string
	}{
		{"target at 15:00", 15, "・下午茶：「飯、湯」→ 併入原有 400 → 1260 大卡", "・下午茶 1260 大卡"},
		{"target at 20:00", 20, "・點心：「飯、湯」→ 併入原有 400 → 1260 大卡", "・點心 1260 大卡"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newEditFixture(t)
			src := f.seedF(t, regroupUser)
			f.seedSlot(t, regroupUser, "snack", "蛋糕", rgToday(c.hour, 0), 400)
			ok := f.offerRegroup(t, regroupUser, src.ID, planTailItemsToSnack)
			mustContain(t, replyText(t, f), c.card)

			_, reply := f.tapWith(t, ok, nil)

			mustContain(t, reply, c.result)
		})
	}
}

// R5：來源是點心時，確認卡（regroup_build 的 srcLabel）與成功訊息（record_edit_postback 的 srcLabel）都用來源自己的時間.
func TestRegroupSourceLabelFollowsSourceEatenAt(t *testing.T) {
	for _, c := range []struct {
		name       string
		hour       int
		card, done string
	}{
		{"source at 15:00", 15, "・下午茶：改為早餐（400 大卡不變）", "・早餐 400 大卡（原為下午茶）"},
		{"source at 20:00", 20, "・點心：改為早餐（400 大卡不變）", "・早餐 400 大卡（原為點心）"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newEditFixture(t)
			src := f.seedSlot(t, regroupUser, "snack", "蛋糕", rgToday(c.hour, 0), 400)
			ok := f.offerRegroup(t, regroupUser, src.ID, planToBreakfastInPlace)
			mustContain(t, replyText(t, f), c.card)

			_, reply := f.tapWith(t, ok, nil)

			mustContain(t, reply, c.done)
		})
	}
}
