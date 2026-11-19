package liff

import (
	"bytes"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	"chatbot-go/internal/storage/database/workout"
	"crypto/sha256"
	"encoding/hex"
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

// ---- PR A（F30-03 / F30-05 / F30-06 / F30-07）新增端點的契約：逐字比對 testdata；既有四份 testdata 不得被改 ----

// 本檔新增的 package 層級名字一律 acct 前綴（D6b）.
func TestAcctContractNewEndpointsMatchGoldenFiles(t *testing.T) {
	f := acctNewFixture(t)
	acctSeedMonthContract(f)
	f.acctSeedSub("U1", "weather_warning", true, "台北市", "新北市板橋區")
	f.acctSeedSub("U1", "earthquake_tsunami", true)
	f.users.docs["U1"] = acctUserstoreUser("Name-U1", "https://example.test/U1.png")
	for _, c := range []struct{ method, path, body, token, golden string }{
		{http.MethodPost, acctSyncPath, "", "good-U1", "account.json"},
		{http.MethodGet, "/api/v1/me/account", "", "good-U1", "account.json"},
		{http.MethodGet, "/api/v1/me/account", "", "good-U9", "account_empty.json"},
		{http.MethodGet, acctAlertsPath, "", "good-U1", "alerts.json"},
		{http.MethodPut, acctWeatherPath, `{"regions":["新北市板橋區","台北市"]}`, "good-U1", "alert_item.json"},
		{http.MethodGet, acctMonthPath + "2026-11", "", "good-U1", "month.json"},
	} {
		acctGolden(t, f.do(c.method, c.path, c.token, c.body), c.golden)
	}
}

// [F34-53] day.json 與 scale.json 因 view 新增一律輸出的 version 而改雜湊（TestPutMealItemsUpdatesStatsImmediately 守 version 鍵）.
// A20：既有 day / summary / profile / scale 的 testdata 位元組不變（要刻意改格式時，同時更新前端與這裡的雜湊）.
func TestAcctExistingContractFilesAreByteIdentical(t *testing.T) {
	want := map[string]string{
		"day.json":     "9382a4b24650b38a98f5d2b8460249d4ba20e7a86fce2cd1b7e1bfe5f94f5fe4",
		"summary.json": "cb4008c0d95a2ed1441b623026d2da6c9f77bd9eff1b78e3e583e714c87f6a3b",
		"profile.json": "d85ab259c5b6189aab8ed444b15ed2022cc89cd6be4a11d693afe3f37b882f43",
		"scale.json":   "1acc9f22ce92e6921e775f7f3531fec3eea656d553e3d6cb572be99a8cbeb0d7",
	}
	for name, hash := range want {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != hash {
			t.Errorf("%s changed: sha256 %s, want %s", name, got, hash)
		}
	}
}

func acctUserstoreUser(name, picture string) userstore.User {
	return userstore.User{ID: "U1", DisplayName: name, PictureURL: picture}
}
