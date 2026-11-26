package fitness

import (
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseLenientInputs(t *testing.T) {
	cases := []struct {
		in       string
		activity string
		minutes  float64
		km       float64
		kcal     float64
	}{
		{"跑步 5公里 30分鐘", ActivityRunning, 30, 5, 0},
		{"跑5k 30分", ActivityRunning, 30, 5, 0},
		{"今天慢跑半小時", ActivityRunning, 30, 0, 0},
		{"重訓 1小時20分", "重訓", 80, 0, 0},
		{"重訓一個半小時", "重訓", 90, 0, 0},
		{"騎車 20km", ActivityCycling, 0, 20, 0},
		{"游泳三十分鐘", "游泳", 30, 0, 0},
		{"瑜伽 45 min", "瑜珈", 45, 0, 0},
		{"跑步３０分鐘", ActivityRunning, 30, 0, 0},
		{"半馬 2小時", ActivityRunning, 120, 21.1, 0},
		{"打羽球 1.5小時 消耗 600 大卡", "羽球", 90, 0, 600},
		{"散步 3000公尺", ActivityWalking, 0, 3, 0},
		{"重訓 1小時半", "重訓", 90, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p := Parse(tc.in)
			if p.Activity == nil || p.Activity.Name != tc.activity {
				t.Fatalf("activity = %+v", p.Activity)
			}
			if p.DurationMin != tc.minutes || p.DistanceKM != tc.km || p.Calories != tc.kcal {
				t.Errorf("got %v min %v km %v kcal", p.DurationMin, p.DistanceKM, p.Calories)
			}
		})
	}
}

func TestPlaceNamesAreNotWorkouts(t *testing.T) {
	for _, place := range []string{"走馬瀨", "跑馬古道", "信義區", "游"} {
		p := Parse(place)
		if p.HasAmount() || IsActivityOnly(place) {
			t.Errorf("%q must fall through to weather lookup", place)
		}
	}
	for _, button := range []string{"跑步", "重訓", "記游泳", "HIIT"} {
		if !IsActivityOnly(button) {
			t.Errorf("%q should start the duration question", button)
		}
	}
}

func TestBurned(t *testing.T) {
	run := ActivityByName("跑步")
	// 5 公里 30 分鐘 = 10 km/h → MET 9.8；70 公斤 → 9.8×70×0.5 = 343.
	if got := Burned(run, 30, 5, 70); got != 343 {
		t.Errorf("run = %v", got)
	}
	if got := Burned(run, 0, 5, 70); got != 350 {
		t.Errorf("distance only = %v", got)
	}
	if got := Burned(ActivityByName("重訓"), 60, 0, 0); got != 300 {
		t.Errorf("default weight = %v", got)
	}
	if got := Burned(ActivityByName("重訓"), 0, 3, 70); got != 0 {
		t.Errorf("no duration for non-distance sport = %v", got)
	}
	if ActivityByName("沙灘排球趴").Name != "排球" || ActivityByName("劍道").Name != ActivityOther {
		t.Error("ActivityByName mapping")
	}
}

func TestDailyBase(t *testing.T) {
	male := &profile.Profile{WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}
	// BMR = 700 + 1093.75 - 150 + 5 = 1648.75 → ×1.2 = 1978.5 → 1979.
	if got := DailyBase(male); got != 1979 {
		t.Errorf("male = %v", got)
	}
	if DailyBase(&profile.Profile{WeightKG: 70}) != 0 || DailyBase(nil) != 0 {
		t.Error("incomplete profile should be 0")
	}
}

func TestParseProfile(t *testing.T) {
	u, ok := ParseProfile("體重 70.5 身高170 30歲 男")
	if !ok || u.WeightKG != 70.5 || u.HeightCM != 170 || u.Age != 30 || u.Sex != profile.SexMale {
		t.Errorf("got %+v", u)
	}
	if u, ok := ParseProfile("我現在 65公斤"); !ok || u.WeightKG != 65 {
		t.Errorf("unit form: %+v", u)
	}
	if _, ok := ParseProfile("體重 999"); ok {
		t.Error("out of range must be ignored")
	}
	if _, ok := ParseProfile("跑步 30分鐘"); ok {
		t.Error("workout text is not a profile")
	}
	p := &profile.Profile{WeightKG: 60, Age: 25}
	u, _ = ParseProfile("體重 58")
	u.Apply(p)
	if p.WeightKG != 58 || p.Age != 25 {
		t.Errorf("apply keeps other fields: %+v", p)
	}
}

// 今日統計要列出今天每一餐吃了什麼（餐別、品項、該餐大卡），不只是總數.
func TestTodayStatsListsEachMeal(t *testing.T) {
	// [F40-21]
	taipei := time.FixedZone("Asia/Taipei", 8*3600)
	meals := []meal.Meal{
		{
			EatenAt: time.Date(2026, 11, 26, 12, 22, 0, 0, taipei), MealType: "lunch",
			Items: []meal.FoodItem{{Name: "雞腿便當"}, {Name: "燙青菜"}}, Total: meal.Nutrients{Calories: 720},
		},
		{
			EatenAt: time.Date(2026, 11, 26, 19, 5, 0, 0, taipei), MealType: "dinner",
			Items: []meal.FoodItem{{Name: "牛肉麵"}}, Total: meal.Nutrients{Calories: 650},
		},
	}

	today := FormatToday(meals, nil, 1869)

	for _, want := range []string{"攝取：1370 大卡（2 餐）\n", "・午餐 雞腿便當、燙青菜：720 大卡\n", "・晚餐 牛肉麵：650 大卡\n"} {
		if !strings.Contains(today, want) {
			t.Errorf("today missing %q:\n%s", want, today)
		}
	}
	if strings.Index(today, "雞腿便當") > strings.Index(today, "運動消耗") {
		t.Errorf("the meal list must sit under the intake line, before exercise:\n%s", today)
	}
}

// 今日統計一天記很多餐時整則仍在 LINE 文字上限 5000 字內:第一餐照列,說還有幾餐沒列,熱量收支照樣算全部.
func TestTodayStatsCapsLongMealLists(t *testing.T) {
	// [F43-03]
	items := make([]meal.FoodItem, 20)
	for i := range items {
		items[i] = meal.FoodItem{Name: strings.Repeat("雞", 30)}
	}
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.FixedZone("Asia/Taipei", 8*3600))
	meals := make([]meal.Meal, 90)
	for i := range meals {
		meals[i] = meal.Meal{MealType: "lunch", EatenAt: at, Items: items, Total: meal.Nutrients{Calories: 100}}
	}

	today := FormatToday(meals, nil, 1869)

	if n := utf8.RuneCountInString(today); n > 5000 || !strings.Contains(today, "攝取：9000 大卡（90 餐）\n・午餐 雞") ||
		!strings.Contains(today, "還有 ") || !strings.Contains(today, " 餐未列出") || !strings.Contains(today, "淨值：") {
		t.Errorf("today must stay within 5000 runes, keep the totals, list the first meal and say how many are left out (got %d runes):\n%.300s", n, today)
	}
}

// 今日統計一天記很多筆運動、品名全是 emoji 時,整則仍在 LINE 上限 5000 字內(LINE 以 UTF-16 計字,emoji 算 2):
// 第一筆運動照列、最後說還有幾項沒列,運動消耗總和仍算全部.
func TestTodayStatsCapsLongWorkouts(t *testing.T) {
	// [F43-06]
	items := make([]meal.FoodItem, 20)
	for i := range items {
		items[i] = meal.FoodItem{Name: strings.Repeat("🍗", 30)}
	}
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.FixedZone("Asia/Taipei", 8*3600))
	meals := make([]meal.Meal, 90)
	for i := range meals {
		meals[i] = meal.Meal{MealType: "lunch", EatenAt: at, Items: items, Total: meal.Nutrients{Calories: 100}}
	}
	workouts := make([]workout.Workout, 600)
	for i := range workouts {
		workouts[i] = workout.Workout{Activity: ActivityRunning, DurationMin: 30, DistanceKM: 5, Calories: 10, DoneAt: at}
	}

	today := FormatToday(meals, workouts, 1869)

	if n := nutrition.LineTextLen(today); n > 5000 || !strings.Contains(today, "運動消耗：6000 大卡（跑步 30 分鐘・5 公里、") ||
		!strings.Contains(today, "…還有 ") || !strings.Contains(today, " 項）") || !strings.Contains(today, "淨值：") {
		t.Errorf("today must stay within 5000 LINE chars, list the first workout, count the rest and keep the totals (got %d):\n%.300s", n, today)
	}
}

func TestSummarizeAndFormat(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	days := []time.Time{day.AddDate(0, 0, -1), day}
	meals := []meal.Meal{{EatenAt: day.Add(12 * time.Hour), Total: meal.Nutrients{Calories: 800}}}
	workouts := []workout.Workout{
		{Activity: ActivityRunning, DurationMin: 30, DistanceKM: 5, Calories: 343, DoneAt: day.Add(-5 * time.Hour)},
		{Activity: "重訓", DurationMin: 60, Calories: 300, DoneAt: day.Add(18 * time.Hour)},
	}
	stats := Summarize(days, meals, workouts)
	if stats[0].Burned != 343 || stats[0].RunKM != 5 || stats[1].Intake != 800 || stats[1].Burned != 300 {
		t.Fatalf("stats = %+v", stats)
	}
	week := FormatWeek(stats)
	for _, want := range []string{"近 7 天統計", "攝取 800 大卡、運動消耗 643 大卡（2 次）", "跑步總距離：5 公里"} {
		if !strings.Contains(week, want) {
			t.Errorf("week missing %q:\n%s", want, week)
		}
	}
	today := FormatToday(meals, workouts[1:], 1979)
	for _, want := range []string{"攝取：800 大卡（1 餐）", "運動消耗：300 大卡（重訓 60 分鐘）", "淨值：-1479 大卡（熱量赤字）"} {
		if !strings.Contains(today, want) {
			t.Errorf("today missing %q:\n%s", want, today)
		}
	}
	if !strings.Contains(FormatToday(nil, nil, 0), "設定「體重 70") {
		t.Error("should hint to set profile")
	}
	rec := FormatRecorded(&workouts[0], 0, 343, false)
	if !strings.Contains(rec, "跑步 30 分鐘・5 公里") || !strings.Contains(rec, "以 60 公斤估算") {
		t.Errorf("recorded:\n%s", rec)
	}
}

// [F36-05] AIParser 已刪除；LooksLikeWorkout 仍是 stepWorkout 判斷「像在記運動」的依據，斷言從舊 TestAIParser 搬來.
// [F37-01] 運動名邊界（運動、訓練、重訓、健身、騎車、騎腳踏車算；單字練、騎不算）.
func TestLooksLikeWorkout(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// [F37-01] 要有運動名才算；正例
		{"今天運動了一小時", true},
		{"騎腳踏車半小時", true},
		{"騎車去上班", true},
		{"今天訓練了一小時", true},
		{"今天重訓一小時", true},
		{"今天去健身", true},
		{"跑了一下", true},
		{"打了一場球", true},
		{"下班去划了二十分鐘船", true},
		{"走路去吃牛肉麵", true},
		{"JOGGING", true},
		{"ＹＯＧＡ", true},
		// [F37-01] 反例：單字提示字、時間/距離字不再算
		{"路好難走", false},
		{"我等了兩個小時", false},
		{"我等了2小時", false},
		{"等了１０分鐘", false},
		{"距離三公里", false},
		{"今天練琴一小時", false},
		{"騎樓好熱", false},
		{"地球好大", false},
		{"操場在哪", false},
		{"跳過這題", false},
		{"爬蟲", false},
		{"舞台", false},
		{"游先生說", false},
		{"今天練了一下", false},
		{"信義區", false},
		{"", false},
	}
	for _, c := range cases {
		if got := LooksLikeWorkout(c.text); got != c.want {
			t.Errorf("LooksLikeWorkout(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestPlausible(t *testing.T) {
	cases := []struct {
		duration, distance, calories float64
		want                         bool
	}{
		{30, 5, 343, true},
		{24 * 60, 500, 10000, true},
		{24*60 + 1, 0, 100, false},
		{60, 501, 100, false},
		{60, 5, 10001, false},
	}
	for _, c := range cases {
		if got := Plausible(c.duration, c.distance, c.calories); got != c.want {
			t.Errorf("Plausible(%v, %v, %v) = %v, want %v", c.duration, c.distance, c.calories, got, c.want)
		}
	}
}
