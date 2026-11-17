package recordedit

import (
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 範圍（規格 §5）；整餐與單品上限取自 meal 套件，維持單一出處.
const (
	maxGrams       = 2000
	maxPer100G     = 900
	maxMacroGrams  = 500
	minItemKcal    = meal.MinEditCalories
	maxItemKcalVal = meal.MaxItemCalories
	maxTotalKcal   = meal.MaxTotalCalories
)

func outOfRange(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOutOfRange, fmt.Sprintf(format, args...))
}

// copyMeal 深拷貝餐點（品項與其 Base），試算不得動到呼叫端的紀錄.
func copyMeal(m *meal.Meal) *meal.Meal {
	c := *m
	c.Items = make([]meal.FoodItem, len(m.Items))
	for i := range m.Items {
		c.Items[i] = m.Items[i]
		if b := m.Items[i].Base; b != nil {
			nb := *b
			c.Items[i].Base = &nb
		}
	}
	return &c
}

// BuildMealProposal [F33-13] 在餐點副本上試算 plan 並產生提議；入參 m 不被修改.
// 超出範圍回包裝 ErrOutOfRange 的錯誤；沒有任何改變回 ErrNoChange；餐點不支援的 op 回 ErrInvalidPlan.
func BuildMealProposal(plan *Plan, m *meal.Meal) (*Proposal, error) {
	if plan == nil || m == nil {
		return nil, invalid("nil plan or meal")
	}
	work := copyMeal(m)
	before := m.Total.Calories
	if plan.Op == OpDeleteMeal {
		return &Proposal{
			Kind: KindMeal, Op: plan.Op, RecordID: m.ID, BaseVersion: m.Version, Plan: *plan,
			Before: before, After: 0,
			Summary: fmt.Sprintf("要刪除 %s（合計 %.0f 大卡）嗎？刪除後無法復原。", MealLabel(m), before),
		}, nil
	}
	if err := applyMealOp(plan, work); err != nil {
		return nil, err
	}
	if mealUnchanged(m, work) {
		return nil, ErrNoChange
	}
	return &Proposal{
		Kind: KindMeal, Op: plan.Op, RecordID: m.ID, BaseVersion: m.Version, Plan: *plan,
		Before: before, After: work.Total.Calories, Summary: mealSummary(plan, m, work),
	}, nil
}

// ApplyMeal [F33-13] 把提議套到（最新讀出的）餐點；失敗時 m 不被改動.
// 刪除類提議不經這裡（由呼叫端 DeleteByID）；紀錄是否被改過（Version）由呼叫端先比對 BaseVersion.
func (p *Proposal) ApplyMeal(m *meal.Meal) error {
	if p == nil || m == nil || p.Kind != KindMeal {
		return invalid("not a meal proposal")
	}
	work := copyMeal(m)
	if err := applyMealOp(&p.Plan, work); err != nil {
		return err
	}
	*m = *work
	return nil
}

// applyMealOp 把 plan 套在 m 上（m 必須是副本）；所有範圍檢查都在這裡，不信任 plan 內任何數字.
func applyMealOp(plan *Plan, m *meal.Meal) error {
	idx := plan.Item - 1
	switch plan.Op {
	case OpSetTotalKcal:
		if !m.SetTotalCalories(plan.Kcal) {
			return outOfRange("整餐熱量需在 %d–%d 大卡", minItemKcal, maxTotalKcal)
		}
	case OpSetItemKcal:
		return setItemKcal(m, idx, plan.Kcal)
	case OpSetItemByWeight:
		// 熱量由程式乘；plan.Kcal（AI 順手附的數字）一律不採用.
		if !(plan.Grams >= 1 && plan.Grams <= maxGrams) || !(plan.KcalPer100G >= 1 && plan.KcalPer100G <= maxPer100G) {
			return outOfRange("重量需在 1–%d 公克、每 100 公克熱量需在 1–%d 大卡", maxGrams, maxPer100G)
		}
		return setItemKcal(m, idx, math.Round(plan.Grams*plan.KcalPer100G/100))
	case OpScale:
		if !m.SetScale(plan.Factor) {
			return outOfRange("份量倍數需在 %.2f–%d 倍", meal.MinScale, meal.MaxScale)
		}
	case OpAddItem:
		return addItem(m, plan.NewItem)
	case OpRemoveItem:
		return removeItem(m, idx)
	case OpRenameItem:
		return renameItem(m, idx, plan.Name, plan.Portion)
	default:
		return invalid("op %q is not a meal edit", plan.Op)
	}
	return nil
}

func setItemKcal(m *meal.Meal, idx int, kcal float64) error {
	if idx < 0 || idx >= len(m.Items) {
		return outOfRange("沒有第 %d 個品項", idx+1)
	}
	if !m.SetItemCalories(idx, kcal) {
		return outOfRange("單品熱量需在 %d–%d 大卡", minItemKcal, maxItemKcalVal)
	}
	return nil
}

// addItem 新品項過與 nutrition.cleanFood 等價的檢查；新總量 = 原值 + 品項熱量（品項 ≥1，結構上必然變大）.
func addItem(m *meal.Meal, ni *NewItem) error {
	if ni == nil {
		return invalid("add_item without new_item")
	}
	name := cleanText(ni.Name)
	kcal := math.Round(ni.Calories)
	switch {
	case name == "" || utf8.RuneCountInString(name) > MaxNameRunes || nutrition.ContainsURL(name) || !strings.ContainsFunc(name, unicode.IsLetter):
		return outOfRange("新品項名稱需為 1–%d 字的食物名稱", MaxNameRunes)
	case math.IsNaN(kcal) || kcal < minItemKcal || kcal > maxItemKcalVal:
		return outOfRange("新品項熱量需在 %d–%d 大卡", minItemKcal, maxItemKcalVal)
	}
	before := m.Total.Calories
	if before+kcal > maxTotalKcal {
		return outOfRange("整餐熱量不可超過 %d 大卡", maxTotalKcal)
	}
	portion := SanitizeDisplay(ni.Portion, MaxPortionRunes)
	if nutrition.ContainsURL(portion) {
		portion = ""
	}
	m.Rebase()
	m.Items = append(m.Items, meal.FoodItem{
		Name: name, Portion: portion,
		Nutrients: meal.Nutrients{Calories: kcal, ProteinG: macro(ni.ProteinG), CarbsG: macro(ni.CarbsG), FatG: macro(ni.FatG)},
	})
	m.Recalc()
	// 舊資料的品項熱量加總可能和合計不一致；以「原合計 + 新品項」為準，保證新總量嚴格大於原值.
	m.Total.Calories = before + kcal
	return nil
}

func macro(v float64) float64 {
	if v < 0 || v > maxMacroGrams || math.IsNaN(v) {
		return 0
	}
	return math.Round(v*10) / 10
}

func removeItem(m *meal.Meal, idx int) error {
	if idx < 0 || idx >= len(m.Items) {
		return outOfRange("沒有第 %d 個品項", idx+1)
	}
	if len(m.Items) <= 1 {
		return ErrLastItem
	}
	m.Rebase()
	m.Items = append(m.Items[:idx:idx], m.Items[idx+1:]...)
	m.Recalc()
	return nil
}

func renameItem(m *meal.Meal, idx int, name, portion string) error {
	if idx < 0 || idx >= len(m.Items) {
		return outOfRange("沒有第 %d 個品項", idx+1)
	}
	name, portion = SanitizeDisplay(name, MaxNameRunes), SanitizeDisplay(portion, MaxPortionRunes)
	if name != "" && (nutrition.ContainsURL(name) || !strings.ContainsFunc(name, unicode.IsLetter)) {
		return outOfRange("品項名稱需為 1–%d 字的食物名稱", MaxNameRunes)
	}
	if nutrition.ContainsURL(portion) {
		portion = ""
	}
	if name != "" {
		m.Items[idx].Name = name
	}
	if portion != "" {
		m.Items[idx].Portion = portion
	}
	return nil
}

// mealUnchanged 試算結果與原紀錄在品項（名稱、份量、營養素）與份量倍數上完全相同.
func mealUnchanged(a, b *meal.Meal) bool {
	if len(a.Items) != len(b.Items) || a.CurrentScale() != b.CurrentScale() || a.Total != b.Total {
		return false
	}
	for i := range a.Items {
		x, y := &a.Items[i], &b.Items[i]
		if x.Name != y.Name || x.Portion != y.Portion || x.Nutrients != y.Nutrients {
			return false
		}
	}
	return true
}

// MealLabel 「日期 餐別」顯示文字（台北時間，例如「11/17 午餐」）.
func MealLabel(m *meal.Meal) string {
	return m.EatenAt.In(nutrition.Taipei).Format("01/02") + " " + nutrition.MealType(m.MealType).Label()
}

// mealName 確認卡裡稱呼這餐：品項名以「、」串起，已清理並截斷.
func mealName(m *meal.Meal) string {
	names := make([]string, 0, len(m.Items))
	for i := range m.Items {
		names = append(names, m.Items[i].Name)
	}
	if len(names) == 0 {
		return MealLabel(m)
	}
	return SanitizeDisplay(strings.Join(names, "、"), MaxNameRunes)
}

func itemName(m *meal.Meal, idx int) string {
	if idx < 0 || idx >= len(m.Items) {
		return mealName(m)
	}
	return SanitizeDisplay(m.Items[idx].Name, MaxNameRunes)
}

// basisLine 依據一行（已清理、置於引號內當資料顯示）；沒有依據就不寫.
func basisLine(basis string) string {
	basis = SanitizeDisplay(basis, MaxBasisRunes)
	if basis == "" {
		return ""
	}
	return "\n依據：「" + basis + "」"
}

// mealSummary 確認卡文字（程式樣板，不含 AI 自由句；數字取自試算結果）.
func mealSummary(plan *Plan, old, now *meal.Meal) string {
	before, after := old.Total.Calories, now.Total.Calories
	idx := plan.Item - 1
	switch plan.Op {
	case OpSetTotalKcal:
		return fmt.Sprintf("要把「%s」從 %.0f 大卡改成 %.0f 大卡嗎？%s", mealName(old), before, after, basisLine(plan.Basis))
	case OpSetItemKcal:
		return fmt.Sprintf("要把「%s」從 %.0f 大卡改成 %.0f 大卡嗎？%s", itemName(old, idx), old.Items[idx].Calories, now.Items[idx].Calories, basisLine(plan.Basis))
	case OpSetItemByWeight:
		basis := plan.Basis
		if basis == "" {
			basis = fmt.Sprintf("約 %.0f 公克 × 每 100 公克 %.0f 大卡", plan.Grams, plan.KcalPer100G)
		}
		return fmt.Sprintf("要把「%s」從 %.0f 大卡改成 %.0f 大卡嗎？%s", itemName(old, idx), old.Items[idx].Calories, now.Items[idx].Calories, basisLine(basis))
	case OpScale:
		return fmt.Sprintf("要把「%s」的份量調成 %g 倍，從 %.0f 大卡改成 %.0f 大卡嗎？%s", mealName(old), plan.Factor, before, after, basisLine(plan.Basis))
	case OpAddItem:
		added := now.Items[len(now.Items)-1]
		return fmt.Sprintf("要在「%s」加上「%s」約 %.0f 大卡嗎？\n熱量 %.0f → %.0f 大卡%s", mealName(old), SanitizeDisplay(added.Name, MaxNameRunes), added.Calories, before, after, basisLine(plan.Basis))
	case OpRemoveItem:
		return fmt.Sprintf("要把「%s」從「%s」拿掉嗎？\n熱量 %.0f → %.0f 大卡%s", itemName(old, idx), mealName(old), before, after, basisLine(plan.Basis))
	case OpRenameItem:
		return fmt.Sprintf("要把品項「%s」改成「%s」嗎？熱量不變（%.0f 大卡）。", itemName(old, idx), itemName(now, idx), before)
	}
	return ""
}
