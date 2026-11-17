package recordedit

import (
	"chatbot-go/internal/storage/database/meal"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func oneItemMeal(kcal float64) *meal.Meal {
	m := &meal.Meal{
		ID: "m-1", UserID: "U1", Version: 3, MealType: "breakfast",
		EatenAt: time.Date(2026, 11, 17, 8, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		Items:   []meal.FoodItem{{Name: "好市多黑麥酸種麵包", Nutrients: meal.Nutrients{Calories: kcal, ProteinG: 4, CarbsG: 20, FatG: 1}}},
	}
	m.Recalc()
	return m
}

func threeItemMeal() *meal.Meal {
	m := &meal.Meal{ID: "m-3", UserID: "U1", Version: 1, Items: []meal.FoodItem{
		{Name: "甲", Nutrients: meal.Nutrients{Calories: 100, ProteinG: 10}},
		{Name: "乙", Nutrients: meal.Nutrients{Calories: 150, CarbsG: 30}},
		{Name: "丙", Nutrients: meal.Nutrients{Calories: 250, FatG: 12}},
	}}
	m.Recalc()
	return m
}

// [F33-13] A1 set_item_kcal：總量與品項精確等於 210、營養素同比例；超範圍回 ErrOutOfRange 且餐點不變.
func TestSetItemKcalRange(t *testing.T) {
	m := oneItemMeal(120)
	p, err := BuildMealProposal(&Plan{Op: OpSetItemKcal, Target: "M1", Item: 1, Kcal: 210}, m)
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if p.Before != 120 || p.After != 210 || p.RecordID != "m-1" || p.BaseVersion != 3 {
		t.Errorf("proposal = %+v", p)
	}
	if !strings.Contains(p.Summary, "120") || !strings.Contains(p.Summary, "210") {
		t.Errorf("summary must show before and after: %q", p.Summary)
	}
	if m.Total.Calories != 120 {
		t.Fatal("Build must not modify its input")
	}
	if err := p.ApplyMeal(m); err != nil || m.Total.Calories != 210 || m.Items[0].Calories != 210 || m.Items[0].ProteinG != 7 {
		t.Errorf("apply: err=%v total=%v item=%+v", err, m.Total.Calories, m.Items[0])
	}
	for _, k := range []float64{0, -5, 3001, math.NaN(), math.Inf(1)} {
		m := oneItemMeal(120)
		_, err := BuildMealProposal(&Plan{Op: OpSetItemKcal, Item: 1, Kcal: k}, m)
		if !errors.Is(err, ErrOutOfRange) || m.Total.Calories != 120 {
			t.Errorf("kcal %v: err=%v, want ErrOutOfRange and unchanged meal", k, err)
		}
	}
	for _, k := range []float64{1, 3000, 210.4} {
		if _, err := BuildMealProposal(&Plan{Op: OpSetItemKcal, Item: 1, Kcal: k}, oneItemMeal(120)); err != nil {
			t.Errorf("kcal %v must be accepted: %v", k, err)
		}
	}
	if _, err := BuildMealProposal(&Plan{Op: OpSetItemKcal, Item: 2, Kcal: 100}, oneItemMeal(120)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("missing item: err=%v", err)
	}
	if _, err := BuildMealProposal(&Plan{Op: OpSetItemKcal, Item: 1, Kcal: 120}, oneItemMeal(120)); !errors.Is(err, ErrNoChange) {
		t.Errorf("same value must be ErrNoChange, got %v", err)
	}
}

// [F33-13] A2 set_total_kcal：合計精確等於要求值；超範圍拒絕.
func TestSetTotalCaloriesExact(t *testing.T) {
	for _, k := range []float64{1, 333, 501, 777, 6000} {
		m := threeItemMeal()
		p, err := BuildMealProposal(&Plan{Op: OpSetTotalKcal, Target: "M1", Kcal: k}, m)
		if err != nil || p.After != k {
			t.Fatalf("kcal %v: err=%v proposal=%+v", k, err, p)
		}
		if err := p.ApplyMeal(m); err != nil || m.Total.Calories != k {
			t.Errorf("kcal %v: applied total %v err %v", k, m.Total.Calories, err)
		}
	}
	for _, k := range []float64{6001, 0, -1} {
		if _, err := BuildMealProposal(&Plan{Op: OpSetTotalKcal, Kcal: k}, threeItemMeal()); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("kcal %v: err=%v", k, err)
		}
	}
	if _, err := BuildMealProposal(&Plan{Op: OpSetTotalKcal, Kcal: 500}, &meal.Meal{}); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("meal without items must be rejected, got %v", err)
	}
}

// [F33-13] A3 依重量：熱量由程式乘（85×245/100=208），無視 AI 附的 kcal；範圍外拒絕.
func TestSetItemByWeightComputedByProgram(t *testing.T) {
	m := oneItemMeal(120)
	p, err := BuildMealProposal(&Plan{Op: OpSetItemByWeight, Target: "M1", Item: 1, Grams: 85, KcalPer100G: 245, Kcal: 210}, m)
	if err != nil || p.After != 208 {
		t.Fatalf("after = %+v err=%v, want 208 computed by program", p, err)
	}
	if !strings.Contains(p.Summary, "85") || !strings.Contains(p.Summary, "245") {
		t.Errorf("summary must show the calculation: %q", p.Summary)
	}
	bad := []Plan{
		{Grams: 0, KcalPer100G: 245}, {Grams: 2001, KcalPer100G: 245},
		{Grams: 85, KcalPer100G: 0}, {Grams: 85, KcalPer100G: 901},
		{Grams: 2000, KcalPer100G: 900}, // 乘出 18000 > 3000
	}
	for i := range bad {
		bad[i].Op, bad[i].Item = OpSetItemByWeight, 1
		if _, err := BuildMealProposal(&bad[i], oneItemMeal(120)); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("case %d (%+v): err=%v", i, bad[i], err)
		}
	}
}

// [F33-13] A4 屬性：calories ∈ [1,3000] 的新品項，新總量恆嚴格大於原值且等於原值加品項熱量（不靠 prompt）.
func TestAddItemTotalStrictlyGreater(t *testing.T) {
	samples := 0
	for k := 1; k <= 3000; k += 13 {
		for _, base := range []*meal.Meal{oneItemMeal(100), legacyMeal()} {
			m := base
			p, err := BuildMealProposal(&Plan{Op: OpAddItem, Target: "M1", NewItem: &NewItem{Name: "起司", Calories: float64(k)}}, m)
			if err != nil {
				t.Fatalf("kcal %d: %v", k, err)
			}
			old := m.Total.Calories
			cp := *m
			cp.Items = append([]meal.FoodItem(nil), m.Items...)
			if err := p.ApplyMeal(&cp); err != nil {
				t.Fatalf("apply %d: %v", k, err)
			}
			if !(cp.Total.Calories > old) || cp.Total.Calories != old+float64(k) || p.After != old+float64(k) {
				t.Fatalf("kcal %d: old %v new %v after %v", k, old, cp.Total.Calories, p.After)
			}
			if len(cp.Items) != len(m.Items)+1 {
				t.Fatalf("kcal %d: item not appended", k)
			}
			samples++
		}
	}
	if samples < 200 {
		t.Fatalf("only %d samples, want >= 200", samples)
	}
}

// legacyMeal 舊資料：品項沒有熱量、只有合計.
func legacyMeal() *meal.Meal {
	return &meal.Meal{ID: "m-old", Version: 1, Items: []meal.FoodItem{{Name: "舊資料"}}, Total: meal.Nutrients{Calories: 820}}
}

// [F33-13] A4 反向：0/-3/NaN/3001 熱量、無名稱、含 URL、無文字、總量 > 6000 → 拒絕且餐點不變.
func TestAddItemRejectsInvalidItem(t *testing.T) {
	cases := []struct {
		name string
		item *NewItem
		base float64
	}{
		{"zero kcal", &NewItem{Name: "起司", Calories: 0}, 100},
		{"rounds to zero", &NewItem{Name: "起司", Calories: 0.4}, 100},
		{"negative", &NewItem{Name: "起司", Calories: -3}, 100},
		{"nan", &NewItem{Name: "起司", Calories: math.NaN()}, 100},
		{"3001", &NewItem{Name: "起司", Calories: 3001}, 100},
		{"no name", &NewItem{Name: "  ", Calories: 70}, 100},
		{"url name", &NewItem{Name: "http://x.co 起司", Calories: 70}, 100},
		{"digits only", &NewItem{Name: "12345", Calories: 70}, 100},
		{"name 31 runes", &NewItem{Name: strings.Repeat("起", 31), Calories: 70}, 100},
		{"total over 6000", &NewItem{Name: "起司", Calories: 3000}, 3001},
		{"nil item", nil, 100},
	}
	for _, c := range cases {
		m := oneItemMeal(c.base)
		_, err := BuildMealProposal(&Plan{Op: OpAddItem, NewItem: c.item}, m)
		if err == nil || (!errors.Is(err, ErrOutOfRange) && !errors.Is(err, ErrInvalidPlan)) {
			t.Errorf("%s: err=%v, want rejection", c.name, err)
		}
		if m.Total.Calories != c.base || len(m.Items) != 1 {
			t.Errorf("%s: meal changed", c.name)
		}
	}
}

// [F33-13] A5 份量倍率等同 SetScale 並可還原；超範圍拒絕.
func TestPlanScaleUsesSetScale(t *testing.T) {
	m := threeItemMeal()
	want := copyMeal(m)
	if !want.SetScale(0.5) {
		t.Fatal("setup")
	}
	p, err := BuildMealProposal(&Plan{Op: OpScale, Target: "M1", Factor: 0.5}, m)
	if err != nil || p.After != want.Total.Calories {
		t.Fatalf("after = %+v err=%v, want %v", p, err, want.Total.Calories)
	}
	if err := p.ApplyMeal(m); err != nil || m.Total != want.Total {
		t.Fatalf("apply: err=%v total=%+v want %+v", err, m.Total, want.Total)
	}
	back, err := BuildMealProposal(&Plan{Op: OpScale, Target: "M1", Factor: 1}, m)
	if err != nil || back.After != 500 {
		t.Errorf("factor 1 must restore 500, got %+v err=%v", back, err)
	}
	for _, f := range []float64{0.1, 3.5, 0} {
		if _, err := BuildMealProposal(&Plan{Op: OpScale, Factor: f}, threeItemMeal()); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("factor %v: err=%v", f, err)
		}
	}
}

// [F33-13] A6 刪品項：合計重算；只剩一項、item=0/99 拒絕.
func TestRemoveItem(t *testing.T) {
	m := threeItemMeal()
	p, err := BuildMealProposal(&Plan{Op: OpRemoveItem, Target: "M1", Item: 2}, m)
	if err != nil || p.After != 350 {
		t.Fatalf("after = %+v err=%v, want 350", p, err)
	}
	if err := p.ApplyMeal(m); err != nil || len(m.Items) != 2 || m.Total.Calories != 350 || m.Items[1].Name != "丙" {
		t.Errorf("apply: err=%v items=%+v total=%v", err, m.Items, m.Total.Calories)
	}
	if _, err := BuildMealProposal(&Plan{Op: OpRemoveItem, Item: 1}, oneItemMeal(120)); !errors.Is(err, ErrLastItem) {
		t.Errorf("last item: err=%v", err)
	}
	for _, i := range []int{0, 99} {
		if _, err := BuildMealProposal(&Plan{Op: OpRemoveItem, Item: i}, threeItemMeal()); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("item %d: err=%v", i, err)
		}
	}
}

// [F33-13] rename 與 delete_meal：rename 不動數字；delete 的提議帶被刪紀錄熱量.
func TestRenameAndDeleteMealProposals(t *testing.T) {
	m := threeItemMeal()
	p, err := BuildMealProposal(&Plan{Op: OpRenameItem, Item: 1, Name: "[[MEAL]]乙\n換行"}, m)
	if err != nil || p.Before != p.After || strings.Contains(p.Summary, "[[") || strings.Contains(p.Summary, "\n換行") {
		t.Fatalf("rename: %+v err=%v", p, err)
	}
	d, err := BuildMealProposal(&Plan{Op: OpDeleteMeal, Target: "M1"}, oneItemMeal(500))
	if err != nil || d.Before != 500 || d.After != 0 || !strings.Contains(d.Summary, "11/17 早餐") || !strings.Contains(d.Summary, "500") {
		t.Errorf("delete: %+v err=%v", d, err)
	}
	if err := d.ApplyMeal(oneItemMeal(500)); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("delete proposals are not applied through ApplyMeal, got %v", err)
	}
	if _, err := BuildMealProposal(&Plan{Op: OpSetWorkout, Target: "W1"}, oneItemMeal(500)); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("workout op on a meal: err=%v", err)
	}
}

// [F33-13] 規格 A4 驗收 ApplyMeal：套用 add_item 提議後 Total 嚴格大於套用前、等於套用前加新品項熱量；失敗時入參不被改動.
func TestApplyMealMatchesProposal(t *testing.T) {
	m := oneItemMeal(100)
	p, err := BuildMealProposal(&Plan{Op: OpAddItem, Target: "M1", NewItem: &NewItem{Name: "起司", Portion: "1片", Calories: 70, ProteinG: 4}}, m)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Total.Calories
	if err := p.ApplyMeal(m); err != nil {
		t.Fatal(err)
	}
	if !(m.Total.Calories > before) || m.Total.Calories != before+70 || m.Total.Calories != p.After {
		t.Errorf("applied total %v, before %v, proposal after %v", m.Total.Calories, before, p.After)
	}
	if m.Items[1].Name != "起司" || m.Items[1].Portion != "1片" {
		t.Errorf("new item not appended: %+v", m.Items)
	}
	// 失敗時入參不被改動：proposal 的計畫在最新紀錄上已不成立（品項被刪光）.
	stale := oneItemMeal(120)
	bad := &Proposal{Kind: KindMeal, Op: OpSetItemKcal, Plan: Plan{Op: OpSetItemKcal, Item: 5, Kcal: 100}}
	snapshot := *stale
	if err := bad.ApplyMeal(stale); !errors.Is(err, ErrOutOfRange) || stale.Total != snapshot.Total || stale.Items[0].Calories != 120 || stale.Version != snapshot.Version {
		t.Errorf("failed apply must leave the meal untouched: err=%v meal=%+v", err, stale)
	}
	if err := (&Proposal{Kind: KindWorkout}).ApplyMeal(stale); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("workout proposal must not apply to a meal: %v", err)
	}
}
