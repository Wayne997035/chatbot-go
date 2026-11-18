//go:build liveeval

package assistant

// [F34-19] 改餐別 / 移品項 / 拆多餐的真模型重播（規格 #30-#32）. 照正式環境的供應商設定呼叫真的 AI；
// 平常的 go test 與 CI 不會編進來，由 user 手動執行（engineer 只編譯、列名，不跑、不讀金鑰檔）：
//   go test -tags liveeval ./internal/assistant/ -run TestLiveRegroup（由 user 在本機帶真金鑰執行）
// 斷言「結果狀態」（把提議套到固定資料 F 之後各餐別的熱量），不斷言模型選了哪個 op 名稱：
// 「改為早餐」可以是 set_meal_type 也可以是 move_items，只要結果是對的就算過.

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"testing"
	"time"
)

// liveRegroupNow 重播的「現在」：台北今天 20:00（晚餐 18:30 之後）.
func liveRegroupNow() time.Time {
	local := time.Now().In(nutrition.Taipei)
	return time.Date(local.Year(), local.Month(), local.Day(), 20, 0, 0, 0, nutrition.Taipei)
}

// liveFixtureF 固定資料 F（取自正式環境那筆）：今天晚餐 18:30、五項 450/200/300/500/360、合計 1810.
func liveFixtureF(now time.Time) meal.Meal {
	names := []string{"牛肉火腿起司三明治", "沙拉", "雞胸肉", "飯", "湯"}
	kcal := []float64{450, 200, 300, 500, 360}
	m := meal.Meal{ID: "dinner", UserID: "U1", Version: 3, MealType: "dinner", EatenAt: now.Add(-90 * time.Minute), Source: meal.SourceText}
	for i := range names {
		m.Items = append(m.Items, meal.FoodItem{Name: names[i], Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal[i]}})
	}
	m.Recalc()
	return m
}

// liveRegroupState 把提議套到資料 F：回傳套用後各餐別的合計熱量（被刪的來源不算）與是否為 regroup 提議.
func liveRegroupState(t *testing.T, o *recordedit.Outcome, src *meal.Meal) (map[string]float64, bool) {
	t.Helper()
	if !isProposal(o) {
		return nil, false
	}
	if o.Proposal.Regroup == nil {
		return nil, true
	}
	writes, err := recordedit.ApplyRegroup(o.Proposal.Regroup, src, nil)
	if err != nil {
		t.Fatalf("ApplyRegroup on the model's proposal: %v", err)
	}
	totals := map[string]float64{}
	for _, w := range writes {
		if w.Meal != nil {
			totals[w.Meal.MealType] += w.Meal.Total.Calories
		}
	}
	return totals, true
}

func liveRegroupOutcome(t *testing.T, say string, history []Turn, meals ...meal.Meal) *recordedit.Outcome {
	t.Helper()
	router, order := liveRouter(t)
	now := liveRegroupNow()
	tc := &editCase{name: "regroup", say: say, meals: meals, history: history}
	a := liveEditAssistant(t, router, order, "", tc, now)
	out, err := a.PlanEdit(context.Background(), "U1", say)
	if err != nil {
		t.Fatalf("PlanEdit: %v", err)
	}
	t.Logf("客人：%s\n    Outcome: kind=%d reason=%q%s", say, out.Kind, out.Reason, describeOutcome(out))
	return out
}

// TestLiveRegroupMoveSandwich [F34-19] 規格 #30：資料 F ＋ 助理先前估算的對話，「把牛肉火腿起司三明治改為早餐」→ 出卡；
// 套用後早餐 450（三明治）、晚餐 1360.
func TestLiveRegroupMoveSandwich(t *testing.T) {
	now := liveRegroupNow()
	src := liveFixtureF(now)
	history := []Turn{{
		Question: "牛肉火腿起司三明治 這樣大概多少大卡",
		Answer:   "牛肉火腿起司三明治一份約 450 大卡。你今天晚餐那筆合計 1810 大卡。",
	}}
	out := liveRegroupOutcome(t, "把牛肉火腿起司三明治改為早餐", history, src)
	totals, ok := liveRegroupState(t, out, &src)
	if !ok || totals == nil {
		t.Fatalf("紅：沒有出 regroup 確認卡（模型回 none 或只出熱量修改）：%+v", out)
	}
	if totals["breakfast"] != 450 || totals["dinner"] != 1360 || len(totals) != 2 {
		t.Errorf("紅：套用後各餐別 = %v，要 breakfast 450、dinner 1360", totals)
	}
}

// TestLiveRegroupWholeAndSplit [F34-19] 規格 #31：「這筆改成午餐」→ 單筆午餐 1810；「幫我分成早午晚三餐」→ ≥2 組、各組熱量和 = 1810、每組餐別互異.
func TestLiveRegroupWholeAndSplit(t *testing.T) {
	now := liveRegroupNow()
	src := liveFixtureF(now)
	t.Run("whole meal to lunch", func(t *testing.T) {
		out := liveRegroupOutcome(t, "這筆改成午餐", nil, src)
		totals, ok := liveRegroupState(t, out, &src)
		if !ok || totals == nil {
			t.Fatalf("紅：沒有出 regroup 確認卡：%+v", out)
		}
		if len(totals) != 1 || totals["lunch"] != 1810 {
			t.Errorf("紅：套用後各餐別 = %v，要單筆 lunch 1810", totals)
		}
	})
	t.Run("split into breakfast lunch dinner", func(t *testing.T) {
		out := liveRegroupOutcome(t, "幫我分成早午晚三餐", nil, src)
		totals, ok := liveRegroupState(t, out, &src)
		if !ok || totals == nil {
			t.Fatalf("紅：沒有出 regroup 確認卡：%+v", out)
		}
		var sum float64
		for _, v := range totals {
			sum += v
		}
		if len(totals) < 2 || sum != 1810 {
			t.Errorf("紅：套用後各餐別 = %v，要 ≥2 個餐別且合計 1810", totals)
		}
		seen := map[string]bool{}
		for _, g := range out.Proposal.Regroup.Groups {
			if seen[g.MealType] {
				t.Errorf("紅：兩組是同一個餐別 %s", g.MealType)
			}
			seen[g.MealType] = true
		}
	})
}

// TestLiveRegroupKcalRegression [F34-19] 規格 #32 迴歸：「把午餐的麵包改成 210 大卡」仍出單品項熱量卡（改的是午餐那筆），不是 regroup.
func TestLiveRegroupKcalRegression(t *testing.T) {
	now := liveRegroupNow()
	lunch := meal.Meal{
		ID: "lunch", UserID: "U1", Version: 1, MealType: "lunch", EatenAt: now.Add(-6 * time.Hour),
		Items: []meal.FoodItem{{Name: "麵包", Nutrients: meal.Nutrients{Calories: 120}}}, Total: meal.Nutrients{Calories: 120},
	}
	out := liveRegroupOutcome(t, "把午餐的麵包改成 210 大卡", nil, liveFixtureF(now), lunch)
	if !isProposal(out) {
		t.Fatalf("紅：沒有出單品項熱量卡：%+v", out)
	}
	p := out.Proposal
	if p.Regroup != nil || recordedit.IsRegroupOp(p.Op) {
		t.Errorf("紅：被誤判成 regroup（op=%s）", p.Op)
	}
	if p.RecordID != "lunch" || p.After != 210 {
		t.Errorf("紅：改到 %s、改完 %.0f 大卡，要改午餐那筆成 210", p.RecordID, p.After)
	}
}
