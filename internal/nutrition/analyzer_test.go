package nutrition

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeCompleter struct {
	answers []string
	got     *ai.Request
}

func (f *fakeCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	f.got = req
	for _, a := range f.answers {
		if req.Accept(a) {
			return ai.Result{Text: a}
		}
	}
	return ai.Result{Errors: []string{"all rejected"}}
}

const foodJSON = `{"kind":"food","foods":[
	{"name":"白飯","portion":"1碗(約200g)","calories":280.4,"protein_g":5.04,"carbs_g":62,"fat_g":0.6},
	{"name":"滷雞腿","portion":"1支","calories":350,"protein_g":30,"carbs_g":5,"fat_g":22}],
	"advice":"蛋白質足夠，記得補充蔬菜","exercise":null}`

func TestParseFood(t *testing.T) {
	a := parseAnalysis("好的，以下是結果：\n" + foodJSON)
	if a == nil || a.Kind != KindFood || len(a.Foods) != 2 {
		t.Fatalf("got %+v", a)
	}
	if a.Foods[0].Calories != 280 || a.Foods[0].ProteinG != 5 {
		t.Errorf("rounding: %+v", a.Foods[0])
	}
	if a.Total.Calories != 630 || a.Total.ProteinG != 35 || a.Total.FatG != 22.6 {
		t.Errorf("total computed in code: %+v", a.Total)
	}
	if a.Advice != "蛋白質足夠，記得補充蔬菜" {
		t.Errorf("advice %q", a.Advice)
	}
}

func TestParseRejectsUnusableAnswers(t *testing.T) {
	cases := map[string]string{
		"not json":           "我看不出來",
		"unknown kind":       `{"kind":"cat"}`,
		"food without items": `{"kind":"food","foods":[]}`,
		"absurd calories":    `{"kind":"food","foods":[{"name":"蛋糕","calories":99999}]}`,
		"nameless food":      `{"kind":"food","foods":[{"name":"","calories":100}]}`,
		"exercise empty":     `{"kind":"exercise","exercise":{"activity":"跑步"}}`,
		"exercise null":      `{"kind":"exercise","exercise":null}`,
	}
	for name, text := range cases {
		if got := parseAnalysis(text); got != nil {
			t.Errorf("%s: should be rejected, got %+v", name, got)
		}
	}
}

func TestParseDropsBadItemsAndClampsMacros(t *testing.T) {
	a := parseAnalysis(`{"kind":"food","foods":[{"name":"湯","calories":-5},{"name":"麵","calories":500,"protein_g":-3,"fat_g":9999}],"advice":"` +
		strings.Repeat("長", 100) + `"}`)
	if a == nil || len(a.Foods) != 1 || a.Foods[0].ProteinG != 0 || a.Foods[0].FatG != 0 {
		t.Fatalf("got %+v", a)
	}
	if a.Advice != "" {
		t.Error("over-long advice should be dropped")
	}
}

func TestParseExerciseAndOther(t *testing.T) {
	a := parseAnalysis(`{"kind":"exercise","foods":[],"exercise":{"activity":"跑步","duration_min":32,"distance_km":5.2,"calories":-1}}`)
	if a == nil || a.Exercise == nil || a.Exercise.DurationMin != 32 || a.Exercise.Calories != 0 {
		t.Fatalf("got %+v", a)
	}
	if a := parseAnalysis(`{"kind":"other","foods":[],"exercise":null}`); a == nil || a.Kind != KindOther {
		t.Errorf("other: %+v", a)
	}
}

func TestAnalyzeImageSkipsRejectedAnswers(t *testing.T) {
	c := &fakeCompleter{answers: []string{"這是一碗飯", foodJSON}}
	a, err := NewAnalyzer(c).AnalyzeImage(context.Background(), ai.Image{MIME: "image/jpeg", Data: []byte{1}}, "")
	if err != nil || len(a.Foods) != 2 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	if len(c.got.Images) != 1 || !c.got.JSON {
		t.Errorf("request = %+v", c.got)
	}
	if _, err := NewAnalyzer(&fakeCompleter{answers: []string{"nope"}}).AnalyzeImage(context.Background(), ai.Image{}, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if NewAnalyzer(nil) != nil {
		t.Error("nil completer means AI disabled")
	}
}

func TestFormatRecordedAndDay(t *testing.T) {
	a := parseAnalysis(foodJSON)
	m := &meal.Meal{Items: a.Foods, Total: a.Total, Advice: a.Advice, Place: &meal.Place{Name: "自助餐", Source: meal.PlaceFromPhoto}}
	text := FormatRecorded(m, "🍱 已記錄午餐"+Disclaimer)
	for _, want := range []string{
		"已記錄午餐", Disclaimer, "• 白飯 1碗(約200g)：280 大卡", "蛋白質 5g｜碳水 62g｜脂肪 0.6g", "合計：630 大卡",
		"📍 自助餐（從照片辨識）", "建議：蛋白質足夠",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	lunch := time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC) // 台灣 12:30
	m.SetScale(0.5)
	if !strings.Contains(FormatRecorded(m, "h"), "合計：315 大卡｜蛋白質 17.5g｜碳水 33.5g｜脂肪 11.3g（份量 ×0.5）") {
		t.Errorf("scaled:\n%s", FormatRecorded(m, "h"))
	}
	day := FormatDay([]meal.Meal{{MealType: "lunch", EatenAt: lunch, Items: a.Foods, Total: a.Total, Place: &meal.Place{Name: "麥當勞"}}})
	if !strings.Contains(day, "午餐 白飯、滷雞腿（麥當勞）：630 大卡") || !strings.Contains(day, "今日飲食（1 餐）") {
		t.Errorf("day:\n%s", day)
	}
	// [F34-79] 不再顯示時間：舊字串必須消失.
	if strings.Contains(day, "12:30") {
		t.Errorf("day still shows the clock:\n%s", day)
	}
	if !strings.Contains(FormatDay(nil), "還沒有飲食紀錄") {
		t.Error("empty day message")
	}
}

func TestDayRangeUsesTaiwanDate(t *testing.T) {
	// UTC 16:30 = 台灣隔天 00:30.
	from, to := DayRange(time.Date(2026, 11, 2, 16, 30, 0, 0, time.UTC))
	if got := from.In(Taipei).Format("2006-01-02 15:04"); got != "2026-11-03 00:00" {
		t.Errorf("from = %s", got)
	}
	if to.Sub(from) != 24*time.Hour {
		t.Errorf("range = %v", to.Sub(from))
	}
}

// dayItem 今日飲食測試用：指定餐別字串與台灣時刻的一筆.
func dayItem(mealType string, h, min int, name string) meal.Meal {
	return meal.Meal{
		MealType: mealType, EatenAt: time.Date(2026, 3, 4, h, min, 0, 0, Taipei),
		Items: []meal.FoodItem{{Name: name}}, Total: meal.Nutrients{Calories: 100},
	}
}

// [F34-79] 今日飲食每行以餐別名開頭（名稱表：早餐/午餐/點心/晚餐/宵夜），不是「下午茶」或英文.
func TestFormatDayLeadsWithMealLabel(t *testing.T) {
	day := FormatDay([]meal.Meal{
		dayItem("breakfast", 8, 0, "蛋餅"), dayItem("lunch", 12, 30, "便當"), dayItem("snack", 15, 30, "餅乾"),
		dayItem("dinner", 18, 30, "拉麵"), dayItem("late_night", 22, 30, "鹹酥雞"),
	})
	lines := strings.Split(day, "\n")
	// 第 0 行是標頭，最後一行是合計.
	for i, prefix := range []string{"早餐 ", "午餐 ", "點心 ", "晚餐 ", "宵夜 "} {
		if got := lines[i+1]; !strings.HasPrefix(got, prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, got, prefix)
		}
	}
	for _, bad := range []string{"下午茶", "snack", "08:00", "12:30", "15:30", "22:30"} {
		if strings.Contains(day, bad) {
			t.Errorf("day must not contain %q:\n%s", bad, day)
		}
	}
}

// [F34-79] 舊紀錄 MealType 空或未知：由 EatenAt 推餐別，不得顯示「一餐」.
func TestFormatDayLegacyMealTypeFallsBackToTime(t *testing.T) {
	day := FormatDay([]meal.Meal{dayItem("", 12, 30, "便當"), dayItem("", 23, 10, "泡麵"), dayItem("brunch", 8, 0, "蛋餅")})
	lines := strings.Split(day, "\n")
	for i, prefix := range []string{"午餐 ", "宵夜 ", "早餐 "} {
		if got := lines[i+1]; !strings.HasPrefix(got, prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, got, prefix)
		}
	}
	if strings.Contains(day, "一餐") {
		t.Errorf("legacy rows must not show 一餐:\n%s", day)
	}
}

// [F34-79] 同日同餐別照列兩行、不合併、順序即輸入（EatenAt）順序.
func TestFormatDayKeepsTwoSameLabelLines(t *testing.T) {
	day := FormatDay([]meal.Meal{dayItem("snack", 10, 30, "珍珠奶茶"), dayItem("snack", 20, 0, "餅乾")})
	lines := strings.Split(day, "\n")
	if !strings.Contains(day, "今日飲食（2 餐）") || len(lines) != 4 {
		t.Fatalf("two records, header + 2 lines + total:\n%s", day)
	}
	if !strings.HasPrefix(lines[1], "點心 珍珠奶茶") || !strings.HasPrefix(lines[2], "點心 餅乾") {
		t.Errorf("order or label wrong:\n%s", day)
	}
}

// [F34-81] 照片 snack_only：只在 food 採用；缺欄、"yes"、1、null 都是 false 且不使解析失敗；"TRUE" 不分大小寫.
func TestParseAnalysisSnackOnly(t *testing.T) {
	foods := `"foods":[{"name":"珍珠奶茶","calories":440}]`
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"food true", `{"kind":"food",` + foods + `,"snack_only":true}`, true},
		{"food string true", `{"kind":"food",` + foods + `,"snack_only":"true"}`, true},
		{"food string TRUE", `{"kind":"food",` + foods + `,"snack_only":"TRUE"}`, true},
		{"food string True", `{"kind":"food",` + foods + `,"snack_only":"True"}`, true},
		{"food missing", `{"kind":"food",` + foods + `}`, false},
		{"food false", `{"kind":"food",` + foods + `,"snack_only":false}`, false},
		{"food yes", `{"kind":"food",` + foods + `,"snack_only":"yes"}`, false},
		{"food number", `{"kind":"food",` + foods + `,"snack_only":1}`, false},
		{"food null", `{"kind":"food",` + foods + `,"snack_only":null}`, false},
		{"exercise ignores true", `{"kind":"exercise","exercise":{"activity":"跑步","duration_min":30},"snack_only":true}`, false},
		{"other ignores true", `{"kind":"other","snack_only":true}`, false},
	}
	for _, tc := range cases {
		a := parseAnalysis(tc.in)
		if a == nil {
			t.Fatalf("%s: must still parse", tc.name)
		}
		if a.SnackOnly != tc.want {
			t.Errorf("%s: SnackOnly = %v, want %v", tc.name, a.SnackOnly, tc.want)
		}
	}
}

// [F34-81] imagePrompt 含規則句關鍵字；範例用佔位寫法而非具體值.
func TestImagePromptSnackRule(t *testing.T) {
	for _, want := range []string{
		"snack_only：kind 是 food 且畫面上的食物全部是零食、點心或飲料（餅乾、洋芋片、糖果、蛋糕、甜點、水果、水煎包、手搖飲、咖啡、飲料等）才填 true；有任何正餐食物（飯、麵、便當、漢堡、三明治等）就填 false；不確定填 false。",
		`"snack_only":true或false,`, "水煎包", "手搖飲", "不確定填 false",
	} {
		if !strings.Contains(imagePrompt, want) {
			t.Errorf("imagePrompt lacks %q", want)
		}
	}
	for _, bad := range []string{`"snack_only":true,`, `"snack_only":false,`} {
		if strings.Contains(imagePrompt, bad) {
			t.Errorf("imagePrompt example must not carry the concrete value %s", bad)
		}
	}
}
