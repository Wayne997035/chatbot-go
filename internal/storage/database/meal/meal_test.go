package meal

import "testing"

func lunch() *Meal {
	m := &Meal{Items: []FoodItem{
		{Name: "牛肉麵", Nutrients: Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}},
		{Name: "燙青菜", Nutrients: Nutrients{Calories: 51, ProteinG: 2.5, CarbsG: 5.3, FatG: 2.1}},
	}}
	m.Recalc()
	return m
}

func TestSetScaleIsRelativeToTheEstimateNotCumulative(t *testing.T) {
	m := lunch()
	if !m.SetScale(0.5) || m.Total.Calories != 351 || m.Items[0].Calories != 325 || m.Items[1].ProteinG != 1.3 {
		t.Fatalf("half: %+v", m.Total)
	}
	if !m.SetScale(0.5) || m.Total.Calories != 351 {
		t.Errorf("pressing 只吃一半 twice must stay half, got %v", m.Total.Calories)
	}
	for _, s := range []float64{1.5, 0.25, 2, 1} {
		m.SetScale(s)
	}
	if m.Items[1].Calories != 51 || m.Items[1].ProteinG != 2.5 || m.Total.Calories != 701 || m.Total.FatG != 22.1 {
		t.Errorf("back to 1 after several changes must equal the estimate exactly: %+v", m.Items)
	}
	if m.SetScale(0.1) || m.SetScale(5) || m.CurrentScale() != 1 {
		t.Error("out-of-range scales must be rejected and leave the meal unchanged")
	}
}

func TestCurrentScaleDefaultsToOne(t *testing.T) {
	if (&Meal{}).CurrentScale() != 1 {
		t.Error("records saved before scales existed count as 1")
	}
}

func TestRebaseMakesCurrentValuesTheNewEstimate(t *testing.T) {
	m := lunch()
	m.SetScale(2)
	m.Items = append(m.Items, FoodItem{Name: "滷蛋", Nutrients: Nutrients{Calories: 75}})
	m.Rebase()
	if m.CurrentScale() != 1 || m.Total.Calories != 1300+102+75 {
		t.Fatalf("rebase: scale=%v total=%v", m.CurrentScale(), m.Total.Calories)
	}
	m.SetScale(0.5)
	if m.Total.Calories != 650+51+38 {
		t.Errorf("half after rebase = %v", m.Total.Calories)
	}
}
