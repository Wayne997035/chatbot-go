package nutrition

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
func dayItem(mealType string, hour, minute int, name string) meal.Meal {
	return meal.Meal{
		MealType: mealType, EatenAt: time.Date(2026, 3, 4, hour, minute, 0, 0, Taipei),
		Items: []meal.FoodItem{{Name: name}}, Total: meal.Nutrients{Calories: 100},
	}
}

// longMealDay 一天 90 餐、每餐 20 個 30 字的品項:逐餐全列會遠超過 LINE 文字訊息 5000 字.
func longMealDay() []meal.Meal {
	items := make([]meal.FoodItem, 20)
	for i := range items {
		items[i] = meal.FoodItem{Name: strings.Repeat("雞", 30)}
	}
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, Taipei)
	meals := make([]meal.Meal, 90)
	for i := range meals {
		meals[i] = meal.Meal{MealType: "lunch", EatenAt: at, Items: items, Total: meal.Nutrients{Calories: 500}}
	}
	return meals
}

// 逐餐清單有字數上限:列出的行加上沒列出的餐數等於全部,列出的部分不超過 3000 字;少量餐點全部列出.
func TestDayMealLinesStopsAtBudget(t *testing.T) {
	// [F43-02]
	meals := longMealDay()
	lines, omitted := DayMealLines(meals)
	used := 0
	for _, l := range lines {
		used += utf8.RuneCountInString(l) + 1
	}
	if len(lines) == 0 || omitted == 0 || len(lines)+omitted != len(meals) || used > 3000 {
		t.Errorf("lines=%d omitted=%d used=%d runes, want some listed, the rest counted, at most 3000", len(lines), omitted, used)
	}
	if few, none := DayMealLines(meals[:2]); len(few) != 2 || none != 0 {
		t.Errorf("two meals must all be listed: lines=%d omitted=%d", len(few), none)
	}
}

// 今日飲食一天記很多餐時整則仍在 LINE 文字上限內,第一餐照列,最後說還有幾餐沒列.
func TestFormatDayCapsLongMealLists(t *testing.T) {
	// [F43-03]
	day := FormatDay(longMealDay())
	lines := strings.Split(day, "\n")
	if n := utf8.RuneCountInString(day); n > 5000 || len(lines) < 3 || !strings.HasPrefix(lines[1], "午餐 雞") ||
		!strings.Contains(day, "還有 ") || !strings.Contains(day, " 餐未列出") {
		t.Errorf("day must stay within 5000 runes, list the first meal and say how many are left out (got %d runes):\n%.300s", n, day)
	}
}

// LINE 以 UTF-16 code unit 計字:emoji 這類 BMP 以外的字算 2,中文與英數算 1.
func TestLineTextLenCountsUTF16(t *testing.T) {
	// [F43-04]
	for _, tt := range []struct {
		in   string
		want int
	}{{"", 0}, {"a雞", 2}, {"🍗", 2}, {"雞🍗a", 4}} {
		if got := LineTextLen(tt.in); got != tt.want {
			t.Errorf("LineTextLen(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// 逐餐清單上限 3000 個 LINE 字,每行另計「・」與換行 2 字:剛好等於上限全列、多 1 字最後一餐不列、空清單沒有行;
// 第一餐單獨就超過上限時仍列出截短的那一行(以「…」結尾),不讓使用者一餐都看不到.
func TestDayMealLinesBoundaries(t *testing.T) {
	// [F43-05]
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, Taipei)
	mk := func(name string) meal.Meal {
		return meal.Meal{MealType: "lunch", EatenAt: at, Items: []meal.FoodItem{{Name: name}}, Total: meal.Nutrients{Calories: 500}}
	}
	cost := func(m meal.Meal) int { return LineTextLen(DayMealLine(&m)) + 2 }

	if lines, omitted := DayMealLines(nil); len(lines) != 0 || omitted != 0 {
		t.Errorf("no meals: lines=%d omitted=%d, want 0 and 0", len(lines), omitted)
	}

	meals := make([]meal.Meal, 0, 10)
	used := 0
	for len(meals) < 9 {
		m := mk(strings.Repeat("🍗", 100))
		used += cost(m)
		meals = append(meals, m)
	}
	fill := 3000 - used - cost(mk(""))
	meals = append(meals, mk(strings.Repeat("雞", fill)))
	if used+cost(meals[9]) != 3000 {
		t.Fatalf("setup: total = %d, want exactly 3000", used+cost(meals[9]))
	}
	if lines, omitted := DayMealLines(meals); len(lines) != 10 || omitted != 0 {
		t.Errorf("exactly 3000 LINE chars must list every meal: lines=%d omitted=%d", len(lines), omitted)
	}
	meals[9] = mk(strings.Repeat("雞", fill+1))
	if lines, omitted := DayMealLines(meals); len(lines) != 9 || omitted != 1 {
		t.Errorf("3001 LINE chars must leave the last meal out: lines=%d omitted=%d", len(lines), omitted)
	}

	lines, omitted := DayMealLines([]meal.Meal{mk(strings.Repeat("雞", 4000)), mk("便當")})
	if len(lines) != 1 || omitted != 1 || !strings.HasPrefix(lines[0], "午餐 雞") ||
		!strings.HasSuffix(lines[0], "…") || LineTextLen(lines[0])+2 > 3000 {
		t.Errorf("a first meal longer than the cap must be shown cut short: lines=%d omitted=%d", len(lines), omitted)
	}
}

// [F34-79] 今日飲食每行以餐別名開頭（名稱表：早餐/午餐/下午茶[15:30 的點心]/晚餐/宵夜），不是英文.
func TestFormatDayLeadsWithMealLabel(t *testing.T) {
	day := FormatDay([]meal.Meal{
		dayItem("breakfast", 8, 0, "蛋餅"), dayItem("lunch", 12, 30, "便當"), dayItem("snack", 15, 30, "餅乾"),
		dayItem("dinner", 18, 30, "拉麵"), dayItem("late_night", 22, 30, "鹹酥雞"),
	})
	lines := strings.Split(day, "\n")
	// 第 0 行是標頭，最後一行是合計.
	for i, prefix := range []string{"早餐 ", "午餐 ", "下午茶 ", "晚餐 ", "宵夜 "} {
		if got := lines[i+1]; !strings.HasPrefix(got, prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, got, prefix)
		}
	}
	for _, bad := range []string{"snack", "08:00", "12:30", "15:30", "22:30"} {
		if strings.Contains(day, bad) {
			t.Errorf("day must not contain %q:\n%s", bad, day)
		}
	}
	// [F35-01] 唯一的點心在 15:30，整份輸出不得再出現「點心」.
	if strings.Contains(day, "點心") {
		t.Errorf("day must not contain 點心:\n%s", day)
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

// [F35-01] A7：同一天兩筆點心依各自時間顯示不同名稱（10:30 點心、15:30 下午茶、20:00 點心）.
func TestFormatDayTwoSnacksDifferentLabels(t *testing.T) {
	day := FormatDay([]meal.Meal{dayItem("snack", 10, 30, "珍珠奶茶"), dayItem("snack", 15, 30, "蛋糕"), dayItem("snack", 20, 0, "餅乾")})
	lines := strings.Split(day, "\n")
	if len(lines) != 5 {
		t.Fatalf("header + 3 lines + total:\n%s", day)
	}
	for i, prefix := range []string{"點心 珍珠奶茶", "下午茶 蛋糕", "點心 餅乾"} {
		if !strings.HasPrefix(lines[i+1], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, lines[i+1], prefix)
		}
	}
}

// [F35-01] A8：舊紀錄 MealType 空，由 EatenAt 推餐別後名稱仍依時間：15:00 → 下午茶、20:00 → 晚餐.
func TestFormatDayLegacySnackAt1500FallsBackToTime(t *testing.T) {
	day := FormatDay([]meal.Meal{dayItem("", 15, 0, "蛋糕"), dayItem("", 20, 0, "餅乾")})
	lines := strings.Split(day, "\n")
	for i, prefix := range []string{"下午茶 ", "晚餐 "} {
		if got := lines[i+1]; !strings.HasPrefix(got, prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, got, prefix)
		}
	}
	for _, bad := range []string{"一餐", "點心"} {
		if strings.Contains(day, bad) {
			t.Errorf("legacy rows must not show %q:\n%s", bad, day)
		}
	}
}

// resultCompleter [F37-34] 回傳固定 Result 的 Completer.
type resultCompleter struct{ res ai.Result }

func (r *resultCompleter) Complete(context.Context, *ai.Request) ai.Result { return r.res }

// [F37-34] ImageRejected 回 ErrImageUnreadable；一般失敗（含沒有 ImageRejected）仍回 ErrUnavailable，兩者不得互相冒充.
func TestAnalyzeImageSeparatesUnreadableFromUnavailable(t *testing.T) {
	tests := []struct {
		name string
		res  ai.Result
		want error
		not  error
	}{
		{"image_rejected", ai.Result{ImageRejected: true, Errors: []string{"x"}}, ErrImageUnreadable, ErrUnavailable},
		{"all_providers_failed", ai.Result{Errors: []string{"x"}, Attempts: 3}, ErrUnavailable, ErrImageUnreadable},
		{"budget_skipped", ai.Result{Skipped: true}, ErrUnavailable, ErrImageUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAnalyzer(&resultCompleter{res: tt.res}).AnalyzeImage(context.Background(), ai.Image{}, "")
			if !errors.Is(err, tt.want) || errors.Is(err, tt.not) {
				t.Fatalf("err = %v, want %v and not %v", err, tt.want, tt.not)
			}
		})
	}
}
