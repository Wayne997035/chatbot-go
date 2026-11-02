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
	a, err := NewAnalyzer(c).AnalyzeImage(context.Background(), ai.Image{MIME: "image/jpeg", Data: []byte{1}})
	if err != nil || len(a.Foods) != 2 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	if len(c.got.Images) != 1 || !c.got.JSON {
		t.Errorf("request = %+v", c.got)
	}
	if _, err := NewAnalyzer(&fakeCompleter{answers: []string{"nope"}}).AnalyzeImage(context.Background(), ai.Image{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if NewAnalyzer(nil) != nil {
		t.Error("nil completer means AI disabled")
	}
}

func TestFormatMealAndDay(t *testing.T) {
	a := parseAnalysis(foodJSON)
	text := FormatMeal(a)
	for _, want := range []string{"餐點分析", Disclaimer, "• 白飯 1碗(約200g)：280 大卡", "蛋白質 5g｜碳水 62g｜脂肪 0.6g", "合計：630 大卡", "建議：蛋白質足夠"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	lunch := time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC) // 台灣 12:30
	day := FormatDay([]meal.Meal{{EatenAt: lunch, Items: a.Foods, Total: a.Total}})
	if !strings.Contains(day, "12:30 白飯、滷雞腿：630 大卡") || !strings.Contains(day, "今日飲食（1 餐）") {
		t.Errorf("day:\n%s", day)
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
