package liff

import (
	"bytes"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"net/http"
	"os"
	"strings"
	"testing"
)

func contractFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPI(t)
	items := []meal.FoodItem{
		{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}},
		{Name: "滷蛋", Portion: "1顆", Nutrients: meal.Nutrients{Calories: 75, ProteinG: 7, CarbsG: 1, FatG: 5}},
	}
	m := meal.Meal{
		ID: "m1", UserID: "U1", EatenAt: fixedNow.Add(-2 * 60 * 60 * 1e9), Items: items, Advice: "多喝水", Source: meal.SourcePhotoText,
		MealType: "lunch", Note: "午餐吃了牛肉麵", Place: &meal.Place{Name: "老張牛肉麵", Address: "台北市", Source: meal.PlaceFromUser}, Version: 3,
	}
	m.Recalc()
	m.SetScale(0.5)
	bare := meal.Meal{ID: "m2", UserID: "U1", EatenAt: fixedNow.Add(-1 * 60 * 60 * 1e9), Items: []meal.FoodItem{{Name: "拿鐵", Nutrients: meal.Nutrients{Calories: 190}}}}
	bare.Recalc()
	f.meals.Items = []meal.Meal{m, bare}
	f.workouts.Items = []workout.Workout{
		{ID: "w1", UserID: "U1", Activity: "跑步", DurationMin: 30, DistanceKM: 5, Calories: 350, Source: workout.SourceText, DoneAt: fixedNow.Add(-3 * 60 * 60 * 1e9)},
		{ID: "w2", UserID: "U1", Activity: "瑜伽", DurationMin: 45, Calories: 150, Source: workout.SourceAI, DoneAt: fixedNow.Add(-4 * 60 * 60 * 1e9)},
	}
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}
	return f
}

// LIFF 頁面依賴的 JSON 格式（testdata/*.json）. API 改用自己的回應型別後，輸出要和原本一字不差；
// 要刻意改格式時，同時更新前端與這裡的檔案.
func TestAPIContractIsUnchanged(t *testing.T) {
	f := contractFixture(t)
	for _, c := range []struct{ method, path, body, golden string }{
		{http.MethodGet, "/api/v1/me/day?date=2026-11-02", "", "day.json"},
		{http.MethodGet, "/api/v1/me/summary?days=2", "", "summary.json"},
		{http.MethodGet, "/api/v1/me/profile", "", "profile.json"},
		{http.MethodPatch, "/api/v1/me/meals/m2", `{"scale":2}`, "scale.json"},
	} {
		rec := f.do(c.method, c.path, "good-U1", c.body)
		want, err := os.ReadFile("testdata/" + c.golden)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(rec.Body.Bytes()), bytes.TrimSpace(want)) {
			t.Errorf("%s %s: status %d\n got: %s\nwant: %s", c.method, c.path, rec.Code, rec.Body.String(), want)
		}
	}
}

// 沒有紀錄的日子回傳空陣列（前端用 .length 與 forEach，不能是 null）.
func TestEmptyDayUsesEmptyArrays(t *testing.T) {
	f := newAPI(t)
	rec := f.do(http.MethodGet, "/api/v1/me/day?date=2026-10-01", "good-U1", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"date":"2026-10-01","meals":[],"workouts":[]}` {
		t.Errorf("empty day = %s", got)
	}
}
