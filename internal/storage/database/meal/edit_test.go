package meal

import (
	"math"
	"testing"
)

func threeItems() *Meal {
	m := &Meal{Items: []FoodItem{
		{Name: "a", Nutrients: Nutrients{Calories: 100, ProteinG: 10, CarbsG: 10, FatG: 1}},
		{Name: "b", Nutrients: Nutrients{Calories: 150, ProteinG: 15, CarbsG: 20, FatG: 5}},
		{Name: "c", Nutrients: Nutrients{Calories: 250, ProteinG: 5, CarbsG: 40, FatG: 10}},
	}}
	m.Recalc()
	return m
}

// [F33-13] 整餐熱量精確等於要求值（含餘數補最大品項）；範圍外與沒有品項不改動.
func TestMealSetTotalCaloriesExactAndRejects(t *testing.T) {
	for _, k := range []float64{1, 2, 7, 333, 500, 501, 999, 6000} {
		m := threeItems()
		if !m.SetTotalCalories(k) || m.Total.Calories != k {
			t.Errorf("SetTotalCalories(%v) total = %v", k, m.Total.Calories)
		}
	}
	for _, k := range []float64{0, -5, 6001, math.NaN(), math.Inf(1)} {
		m := threeItems()
		before := *m
		if m.SetTotalCalories(k) || m.Total != before.Total || m.Items[0].Calories != 100 {
			t.Errorf("SetTotalCalories(%v) must be rejected and leave the meal unchanged", k)
		}
	}
	empty := &Meal{}
	if empty.SetTotalCalories(100) {
		t.Error("meal without items must be rejected")
	}
	legacy := &Meal{Items: []FoodItem{{Name: "舊資料"}, {Name: "舊資料2"}}}
	if !legacy.SetTotalCalories(300) || legacy.Total.Calories != 300 {
		t.Errorf("legacy meal without item calories: total = %v", legacy.Total.Calories)
	}
}

// [F33-13] 單品熱量：該品項營養素同比例縮放、其餘不動、合計重算；範圍外/不存在的品項不改動.
func TestMealSetItemCaloriesScalesMacros(t *testing.T) {
	m := threeItems()
	if !m.SetItemCalories(0, 200) {
		t.Fatal("rejected valid kcal")
	}
	if m.Items[0].Calories != 200 || m.Items[0].ProteinG != 20 || m.Items[1].Calories != 150 || m.Total.Calories != 600 {
		t.Errorf("unexpected result: %+v total %+v", m.Items, m.Total)
	}
	for _, c := range []struct {
		i int
		k float64
	}{{0, 0}, {0, 3001}, {0, math.NaN()}, {-1, 100}, {3, 100}} {
		m := threeItems()
		if m.SetItemCalories(c.i, c.k) || m.Total.Calories != 500 || m.Items[0].Calories != 100 {
			t.Errorf("SetItemCalories(%d,%v) must be rejected without changes", c.i, c.k)
		}
	}
	m = threeItems()
	if !m.SetItemCalories(2, 210.4) || m.Items[2].Calories != 210 {
		t.Errorf("210.4 must round to 210, got %v", m.Items[2].Calories)
	}
}
