package meal

import (
	"math"
	"strings"
	"testing"
)

func fakeURL(s string) bool { return strings.Contains(s, "http://") }

func okItem() FoodItem {
	return FoodItem{Name: "牛肉麵", Portion: "1碗", Nutrients: Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}}
}

// TestValidateItemsBoundaries [F34-54] 每個欄位的邊界：剛好上限通過、上限加一與非有限數被擋，錯誤訊息點名欄位.
func TestValidateItemsBoundaries(t *testing.T) {
	with := func(f func(*FoodItem)) []FoodItem {
		it := okItem()
		f(&it)
		return []FoodItem{it}
	}
	many := func(n int) []FoodItem {
		out := make([]FoodItem, n)
		for i := range out {
			out[i] = okItem()
			out[i].Calories = 100 // 20 項合計 2000，不碰整餐上限
		}
		return out
	}
	type tc struct {
		name  string
		items []FoodItem
		field string // 空 = 期望通過
	}
	cases := []tc{
		{"ok", []FoodItem{okItem()}, ""},
		{"items 1", many(1), ""},
		{"items max", many(MaxItems), ""},
		{"items 0", many(0), FieldItems},
		{"items nil", nil, FieldItems},
		{"items max+1", many(MaxItems + 1), FieldItems},
		{"name max runes", with(func(i *FoodItem) { i.Name = strings.Repeat("麵", MaxNameRunes) }), ""},
		{"name max+1 runes", with(func(i *FoodItem) { i.Name = strings.Repeat("麵", MaxNameRunes+1) }), FieldName},
		{"name empty", with(func(i *FoodItem) { i.Name = "" }), FieldName},
		{"name blank", with(func(i *FoodItem) { i.Name = "   " }), FieldName},
		{"name with url", with(func(i *FoodItem) { i.Name = "看 http://x.co" }), FieldName},
		{"portion empty", with(func(i *FoodItem) { i.Portion = "" }), ""},
		{"portion max runes", with(func(i *FoodItem) { i.Portion = strings.Repeat("碗", MaxPortionRunes) }), ""},
		{"portion max+1 runes", with(func(i *FoodItem) { i.Portion = strings.Repeat("碗", MaxPortionRunes+1) }), FieldPortion},
		{"portion with url", with(func(i *FoodItem) { i.Portion = "http://x.co" }), FieldPortion},
		{"total at the cap", []FoodItem{{Name: "a", Nutrients: Nutrients{Calories: MaxItemCalories}}, {Name: "b", Nutrients: Nutrients{Calories: MaxItemCalories}}}, ""},
		{"total over the cap", []FoodItem{
			{Name: "a", Nutrients: Nutrients{Calories: MaxItemCalories}}, {Name: "b", Nutrients: Nutrients{Calories: MaxItemCalories}},
			{Name: "c", Nutrients: Nutrients{Calories: 1}},
		}, FieldCalories},
	}
	fields := []struct {
		name string
		max  float64
		set  func(*FoodItem, float64)
	}{
		{FieldCalories, MaxItemCalories, func(i *FoodItem, v float64) { i.Calories = v }},
		{FieldProtein, MaxMacroGrams, func(i *FoodItem, v float64) { i.ProteinG = v }},
		{FieldCarbs, MaxMacroGrams, func(i *FoodItem, v float64) { i.CarbsG = v }},
		{FieldFat, MaxMacroGrams, func(i *FoodItem, v float64) { i.FatG = v }},
	}
	for _, f := range fields {
		for _, v := range []struct {
			label string
			val   float64
			bad   bool
		}{
			{"-1", -1, true},
			{"min", 0, false},
			{"max", f.max, false},
			{"max+1", f.max + 1, true},
			{"NaN", math.NaN(), true},
			{"+Inf", math.Inf(1), true},
			{"-Inf", math.Inf(-1), true},
		} {
			field := ""
			if v.bad {
				field = f.name
			}
			cases = append(cases, tc{f.name + " " + v.label, with(func(i *FoodItem) { f.set(i, v.val) }), field})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateItems(c.items, fakeURL)
			switch {
			case c.field == "" && err != nil:
				t.Errorf("want nil, got %v", err)
			case c.field != "" && (err == nil || !strings.Contains(err.Error(), c.field)):
				t.Errorf("want an error naming %q, got %v", c.field, err)
			}
		})
	}

	t.Run("nil containsURL is an error", func(t *testing.T) {
		if err := ValidateItems([]FoodItem{okItem()}, nil); err == nil || !strings.Contains(err.Error(), "containsURL") {
			t.Errorf("got %v", err)
		}
		if err := ValidateEdit([]FoodItem{okItem()}, nil, EditContext{Unchanged: []bool{false}}); err == nil || !strings.Contains(err.Error(), "containsURL") {
			t.Errorf("ValidateEdit: got %v", err)
		}
	})
}

// TestValidateItemsRangesArePinned [F34-54] 範圍常數的值本身要被釘住；邊界矩陣引用常數，改常數不會讓它變紅.
func TestValidateItemsRangesArePinned(t *testing.T) {
	for name, c := range map[string][2]int{
		"MaxItemCalories":  {MaxItemCalories, 3000},
		"MaxTotalCalories": {MaxTotalCalories, 6000},
		"MaxMacroGrams":    {MaxMacroGrams, 500},
		"MaxItems":         {MaxItems, 20},
		"MaxNameRunes":     {MaxNameRunes, 30},
		"MaxPortionRunes":  {MaxPortionRunes, 30},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, want %d", name, c[0], c[1])
		}
	}
}

// TestValidateItemsEditExemptions [F34-54] 編輯既有餐點：沒改的品項免檢查、品項數上限取既有數量、整餐不高於改前就放行.
func TestValidateItemsEditExemptions(t *testing.T) {
	big := FoodItem{Name: "大餐", Nutrients: Nutrients{Calories: 6000}}
	small := okItem()
	tests := []struct {
		name  string
		items []FoodItem
		edit  EditContext
		field string // 空 = 期望通過
	}{
		{"unchanged 6000-kcal item is skipped", []FoodItem{big}, EditContext{Unchanged: []bool{true}, PriorCount: 1, PriorCalories: 6000}, ""},
		{"same item marked changed is checked", []FoodItem{big}, EditContext{Unchanged: []bool{false}, PriorCount: 1, PriorCalories: 6000}, FieldCalories},
		{"22 prior items stay editable", repeat(small, 22), EditContext{Unchanged: make([]bool, 22), PriorCount: 22, PriorCalories: 14300}, ""},
		{"23 items over a 22-item meal", repeat(small, 23), EditContext{Unchanged: make([]bool, 23), PriorCount: 22, PriorCalories: 14300}, FieldItems},
		{"total up from 6000 is rejected", []FoodItem{{Name: "a", Nutrients: Nutrients{Calories: 3000}}, {Name: "b", Nutrients: Nutrients{Calories: 3001}}},
			EditContext{Unchanged: []bool{true, true}, PriorCount: 2, PriorCalories: 6000}, FieldCalories},
		{"legacy total unchanged is allowed", []FoodItem{{Name: "a", Nutrients: Nutrients{Calories: 3000}}, {Name: "b", Nutrients: Nutrients{Calories: 3500}}},
			EditContext{Unchanged: []bool{true, true}, PriorCount: 2, PriorCalories: 6500}, ""},
		{"total over the cap from a small meal", []FoodItem{{Name: "a", Nutrients: Nutrients{Calories: 3000}}, {Name: "b", Nutrients: Nutrients{Calories: 3000}}, {Name: "c", Nutrients: Nutrients{Calories: 1}}},
			EditContext{Unchanged: []bool{false, false, false}, PriorCount: 1, PriorCalories: 100}, FieldCalories},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEdit(tt.items, fakeURL, tt.edit)
			switch {
			case tt.field == "" && err != nil:
				t.Errorf("want nil, got %v", err)
			case tt.field != "" && (err == nil || !strings.Contains(err.Error(), tt.field)):
				t.Errorf("want an error naming %q, got %v", tt.field, err)
			}
		})
	}
	t.Run("Unchanged length mismatch is an error", func(t *testing.T) {
		if err := ValidateEdit([]FoodItem{small}, fakeURL, EditContext{}); err == nil {
			t.Error("want an error")
		}
	})
}

func repeat(it FoodItem, n int) []FoodItem {
	out := make([]FoodItem, n)
	for i := range out {
		out[i] = it
	}
	return out
}
