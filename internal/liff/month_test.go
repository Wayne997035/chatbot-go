package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 本檔的 package 層級名字一律 acct 前綴（D6b）. 單號 F30-07（R6 整月資料與三級 grade）.

const acctMonthPath = "/api/v1/me/month?month="

// acctSeedMonthContract 與 contractFixture 同一份資料：2026-11-02 一格 = 553 / 2 / 500 / 2 / 5，base 1979.
func acctSeedMonthContract(f *acctFixture) {
	items := []meal.FoodItem{
		{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}},
		{Name: "滷蛋", Portion: "1顆", Nutrients: meal.Nutrients{Calories: 75, ProteinG: 7, CarbsG: 1, FatG: 5}},
	}
	m := meal.Meal{ID: "m1", UserID: "U1", EatenAt: fixedNow.Add(-2 * time.Hour), Items: items, Version: 3}
	m.Recalc()
	m.SetScale(0.5)
	bare := meal.Meal{ID: "m2", UserID: "U1", EatenAt: fixedNow.Add(-1 * time.Hour), Items: []meal.FoodItem{{Name: "拿鐵", Nutrients: meal.Nutrients{Calories: 190}}}}
	bare.Recalc()
	f.meals.Items = []meal.Meal{m, bare}
	f.workouts.Items = []workout.Workout{
		{ID: "w1", UserID: "U1", Activity: "跑步", DurationMin: 30, DistanceKM: 5, Calories: 350, DoneAt: fixedNow.Add(-3 * time.Hour)},
		{ID: "w2", UserID: "U1", Activity: "瑜伽", DurationMin: 45, Calories: 150, DoneAt: fixedNow.Add(-4 * time.Hour)},
	}
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}
}

func acctDecodeMonth(t *testing.T, body []byte) MonthView {
	t.Helper()
	var v MonthView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

// A17：2026-11 逐字等於 testdata/month.json（30 天遞增、11-02 為 green、11-01 無飲食為 ""、base 1979）；壞月份 400；2026-02 為 28 天.
func TestAcctMonthView(t *testing.T) {
	f := acctNewFixture(t)
	acctSeedMonthContract(f)
	acctGolden(t, f.do(http.MethodGet, acctMonthPath+"2026-11", "good-U1", ""), "month.json")

	for _, bad := range []string{"2026-13", "2026-1", "abc", "", "2019-12", "2026-12", "2026-11-02", "2026-00", "+2026-11", "2026-11%20"} {
		rec := f.do(http.MethodGet, acctMonthPath+bad, "good-U1", "")
		if rec.Code != http.StatusBadRequest || acctErrCode(t, rec) != "2001" {
			t.Errorf("month=%q: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(http.MethodGet, "/api/v1/me/month", "good-U1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing month param: %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, acctMonthPath+"2020-01", "good-U1", ""); rec.Code != http.StatusOK {
		t.Errorf("lower bound 2020-01 must be allowed: %d", rec.Code)
	}
	feb := acctDecodeMonth(t, f.do(http.MethodGet, acctMonthPath+"2026-02", "good-U1", "").Body.Bytes())
	if len(feb.Days) != 28 {
		t.Errorf("2026-02 has %d days, want 28", len(feb.Days))
	}
	oct := acctDecodeMonth(t, f.do(http.MethodGet, acctMonthPath+"2026-10", "good-U1", "").Body.Bytes())
	if len(oct.Days) != 31 {
		t.Fatalf("2026-10 has %d days, want 31", len(oct.Days))
	}
	for _, d := range oct.Days {
		if d.Intake != 0 || d.Meals != 0 || d.Burned != 0 || d.Workouts != 0 || d.RunKM != 0 || d.Grade != "" {
			t.Errorf("empty month day not zero: %+v", d)
		}
	}
}

// A18：台灣日界（UTC+8）：2026-10-31T16:00Z 是 11-01 00:00 → 計入 11 月；15:59:59Z 不計；11-30T16:00Z 是 12-01 → 不計.
func TestAcctMonthTaiwanDayBoundaries(t *testing.T) {
	f := acctNewFixture(t)
	mk := func(id string, at time.Time) meal.Meal {
		m := meal.Meal{ID: id, UserID: "U1", EatenAt: at, Items: []meal.FoodItem{{Name: "x", Nutrients: meal.Nutrients{Calories: 100}}}}
		m.Recalc()
		return m
	}
	f.meals.Items = []meal.Meal{
		mk("in-first", time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)),
		mk("before", time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC)),
		mk("in-last", time.Date(2026, 11, 30, 15, 59, 59, 0, time.UTC)),
		mk("after", time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC)),
	}
	v := acctDecodeMonth(t, f.do(http.MethodGet, acctMonthPath+"2026-11", "good-U1", "").Body.Bytes())
	total := 0
	for _, d := range v.Days {
		total += d.Meals
	}
	if v.Days[0].Meals != 1 || v.Days[29].Meals != 1 || total != 2 {
		t.Errorf("11-01 meals=%d 11-30 meals=%d total=%d, want 1/1/2", v.Days[0].Meals, v.Days[29].Meals, total)
	}
}

// A19：只含自己的紀錄；?userId 被忽略.
func TestAcctMonthOnlyCallersData(t *testing.T) {
	f := acctNewFixture(t)
	acctSeedMonthContract(f)
	other := meal.Meal{ID: "o1", UserID: "U2", EatenAt: fixedNow, Items: []meal.FoodItem{{Name: "x", Nutrients: meal.Nutrients{Calories: 9999}}}}
	other.Recalc()
	f.meals.Items = append(f.meals.Items, other)
	f.workouts.Items = append(f.workouts.Items, workout.Workout{ID: "ow", UserID: "U2", Activity: "跑步", Calories: 9999, DoneAt: fixedNow})
	rec := f.do(http.MethodGet, acctMonthPath+"2026-11&userId=U2", "good-U1", "")
	if strings.Contains(rec.Body.String(), "9999") {
		t.Errorf("another user's records leaked: %s", rec.Body.String())
	}
	acctGolden(t, rec, "month.json")
	// U2 只看得到自己的那一筆（9999），不會混進 U1 的 553 / 2 筆.
	if v := acctDecodeMonth(t, f.do(http.MethodGet, acctMonthPath+"2026-11", "good-U2", "").Body.Bytes()); v.Days[1].Intake != 9999 || v.Days[1].Meals != 1 || v.Days[1].Workouts != 1 {
		t.Errorf("U2 sees a mix of users: %+v", v.Days[1])
	}
}

// A30：分級邊界與四捨五入；NetBandKcal 是單一具名常數；base=0 / meals=0 → "".
func TestNetGradeBoundaries(t *testing.T) {
	const base = 2000.0
	cases := []struct {
		name   string
		net    float64 // intake - base - burned
		meals  int
		base   float64
		burned float64
		want   string
	}{
		{"-301", -301, 1, base, 0, "green"},
		{"-300", -300, 1, base, 0, "green"},
		{"-299", -299, 1, base, 0, "yellow"},
		{"+299", 299, 1, base, 0, "yellow"},
		{"+300", 300, 1, base, 0, "yellow"},
		{"+301", 301, 1, base, 0, "red"},
		{"-300.4 rounds to -300", -300.4, 1, base, 0, "green"},
		{"+300.5 rounds away from zero to +301", 300.5, 1, base, 0, "red"},
		{"-300.5 rounds away from zero to -301", -300.5, 1, base, 0, "green"},
		{"+300.4 rounds to +300 (a raw compare would say red)", 300.4, 1, base, 0, "yellow"},
		{"-299.6 rounds to -300 (a raw compare would say yellow)", -299.6, 1, base, 0, "green"},
		{"base unset", -301, 1, 0, 0, ""},
		{"no meals even with big deficit", -1000, 0, base, 0, ""},
		{"burned moves red to yellow (+400 -> +100)", 100, 1, base, 300, "yellow"},
		{"burned absent keeps red (+400)", 400, 1, base, 0, "red"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intake := tc.net + tc.base + tc.burned
			if tc.base == 0 {
				intake = tc.net + base // base 0 的情境用同樣的 intake
			}
			if got := netGrade(intake, tc.base, tc.burned, tc.meals); got != tc.want {
				t.Errorf("netGrade(intake=%v base=%v burned=%v meals=%d) = %q, want %q", intake, tc.base, tc.burned, tc.meals, got, tc.want)
			}
		})
	}
	if NetBandKcal != 300 {
		t.Errorf("NetBandKcal = %d, want 300", NetBandKcal)
	}
}

// A31：每日 grade 一律存在且屬四種值；有運動但沒飲食 → ""；DayView（summary / day）不含 grade.
func TestAcctMonthGradeAlwaysPresentAndNotInDayView(t *testing.T) {
	f := acctNewFixture(t)
	acctSeedMonthContract(f)
	f.workouts.Items = append(f.workouts.Items, workout.Workout{ID: "w3", UserID: "U1", Activity: "跑步", Calories: 400, DoneAt: time.Date(2026, 11, 5, 4, 0, 0, 0, time.UTC)})
	rec := f.do(http.MethodGet, acctMonthPath+"2026-11", "good-U1", "")
	var raw struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, d := range raw.Days {
		g, ok := d["grade"].(string)
		if !ok || (g != "green" && g != "yellow" && g != "red" && g != "") {
			t.Errorf("day %v: grade %v", d["date"], d["grade"])
		}
	}
	if raw.Days[4]["workouts"] != float64(1) || raw.Days[4]["grade"] != "" {
		t.Errorf("11-05 has a workout but no meals, grade must be empty: %v", raw.Days[4])
	}
	b, err := json.Marshal(DayView{Date: "2026-11-02"})
	if err != nil || strings.Contains(string(b), "grade") {
		t.Errorf("DayView must not carry grade: %s %v", b, err)
	}
	for _, path := range []string{"/api/v1/me/summary?days=2", "/api/v1/me/day?date=2026-11-02"} {
		if rec := f.do(http.MethodGet, path, "good-U1", ""); strings.Contains(rec.Body.String(), "grade") {
			t.Errorf("%s leaks grade: %s", path, rec.Body.String())
		}
	}
}
