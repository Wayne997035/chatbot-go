package fitness

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"strings"
	"testing"
	"time"
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

type fakeCompleter struct{ answer string }

func (f fakeCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	if !req.Accept(f.answer) {
		return ai.Result{Errors: []string{"rejected"}}
	}
	return ai.Result{Text: f.answer}
}

func TestAIParser(t *testing.T) {
	p := NewAIParser(fakeCompleter{`{"is_workout":true,"activity":"划船機","duration_min":20,"distance_km":0,"calories":0}`})
	got, ok := p.Parse(context.Background(), "下班去划了二十分鐘船")
	if !ok || got.Activity.Name != "划船" || got.DurationMin != 20 {
		t.Errorf("got %+v ok=%v", got, ok)
	}
	if _, ok := NewAIParser(fakeCompleter{`{"is_workout":false}`}).Parse(context.Background(), "跑去哪裡吃"); ok {
		t.Error("not a workout")
	}
	if _, ok := NewAIParser(fakeCompleter{"不是 JSON"}).Parse(context.Background(), "x"); ok {
		t.Error("invalid answer")
	}
	var nilParser *AIParser
	if _, ok := nilParser.Parse(context.Background(), "x"); ok || NewAIParser(nil) != nil {
		t.Error("nil parser")
	}
	if !LooksLikeWorkout("今天練了一下") || LooksLikeWorkout("信義區") {
		t.Error("LooksLikeWorkout")
	}
}
