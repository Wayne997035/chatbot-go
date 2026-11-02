package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"time"
)

// 這裡是 LIFF API 的回應格式（統計頁前端依賴的契約，見 testdata/*.json）. 和資料庫的結構分開：
// 資料庫加欄位（例如版本號、份量基準）不會跑到 API，API 也不會因為儲存方式改變而變形.

// NutrientsView 熱量與三大營養素.
type NutrientsView struct {
	Calories float64 `json:"calories"`
	ProteinG float64 `json:"protein_g"`
	CarbsG   float64 `json:"carbs_g"`
	FatG     float64 `json:"fat_g"`
}

// FoodItemView 一個品項.
type FoodItemView struct {
	Name     string  `json:"name"`
	Portion  string  `json:"portion"`
	Calories float64 `json:"calories"`
	ProteinG float64 `json:"protein_g"`
	CarbsG   float64 `json:"carbs_g"`
	FatG     float64 `json:"fat_g"`
}

// PlaceView 在哪裡吃的.
type PlaceView struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
	Source  string `json:"source"`
}

// MealView 一餐.
type MealView struct {
	ID       string         `json:"id"`
	EatenAt  time.Time      `json:"eatenAt"`
	Items    []FoodItemView `json:"items"`
	Total    NutrientsView  `json:"total"`
	Advice   string         `json:"advice,omitempty"`
	Source   string         `json:"source,omitempty"`
	MealType string         `json:"mealType,omitempty"`
	Note     string         `json:"note,omitempty"`
	Place    *PlaceView     `json:"place,omitempty"`
	Scale    float64        `json:"scale,omitempty"`
}

// WorkoutView 一筆運動.
type WorkoutView struct {
	ID          string    `json:"id"`
	Activity    string    `json:"activity"`
	DurationMin float64   `json:"durationMin"`
	DistanceKM  float64   `json:"distanceKm,omitempty"`
	Calories    float64   `json:"calories"`
	Source      string    `json:"source"`
	DoneAt      time.Time `json:"doneAt"`
}

func toNutrientsView(n *meal.Nutrients) NutrientsView {
	return NutrientsView{Calories: n.Calories, ProteinG: n.ProteinG, CarbsG: n.CarbsG, FatG: n.FatG}
}

func toMealView(m *meal.Meal) MealView {
	items := make([]FoodItemView, len(m.Items))
	for i := range m.Items {
		it := &m.Items[i]
		items[i] = FoodItemView{
			Name: it.Name, Portion: it.Portion,
			Calories: it.Calories, ProteinG: it.ProteinG, CarbsG: it.CarbsG, FatG: it.FatG,
		}
	}
	v := MealView{
		ID: m.ID, EatenAt: m.EatenAt, Items: items, Total: toNutrientsView(&m.Total),
		Advice: m.Advice, Source: m.Source, MealType: m.MealType, Note: m.Note, Scale: m.Scale,
	}
	if m.Place != nil {
		v.Place = &PlaceView{Name: m.Place.Name, Address: m.Place.Address, Source: m.Place.Source}
	}
	return v
}

func toMealViews(meals []meal.Meal) []MealView {
	out := make([]MealView, len(meals))
	for i := range meals {
		out[i] = toMealView(&meals[i])
	}
	return out
}

func toWorkoutViews(workouts []workout.Workout) []WorkoutView {
	out := make([]WorkoutView, len(workouts))
	for i := range workouts {
		w := &workouts[i]
		out[i] = WorkoutView{
			ID: w.ID, Activity: w.Activity, DurationMin: w.DurationMin, DistanceKM: w.DistanceKM,
			Calories: w.Calories, Source: w.Source, DoneAt: w.DoneAt,
		}
	}
	return out
}
