// Package nutrition 以 AI 看圖分析食物營養與運動紀錄截圖.
package nutrition

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// Kind 圖片類型.
type Kind string

const (
	// KindFood 食物或飲料.
	KindFood Kind = "food"
	// KindExercise 運動 App / 手錶的運動紀錄截圖.
	KindExercise Kind = "exercise"
	// KindOther 其他圖片.
	KindOther Kind = "other"
)

// ExerciseReading 從運動截圖讀出的數值；讀不到的欄位為 0.
type ExerciseReading struct {
	Activity    string  `json:"activity"`
	DurationMin float64 `json:"duration_min"`
	DistanceKM  float64 `json:"distance_km"`
	Calories    float64 `json:"calories"`
}

// ImageAnalysis 一張圖片的分析結果.
type ImageAnalysis struct {
	Kind     Kind
	Foods    []meal.FoodItem
	Total    meal.Nutrients
	Advice   string
	Exercise *ExerciseReading
}

// Completer AI 生成（*ai.Router 實作）.
type Completer interface {
	Complete(ctx context.Context, req *ai.Request) ai.Result
}

// Analyzer 圖片分析器.
type Analyzer struct {
	completer Completer
}

// ErrUnavailable AI 暫時無法分析（細節已寫進 log）.
var ErrUnavailable = errors.New("AI 暫時無法分析圖片")

// NewAnalyzer 建立分析器；completer 為 nil 時回傳 nil（AI 未啟用）.
func NewAnalyzer(completer Completer) *Analyzer {
	if completer == nil {
		return nil
	}
	return &Analyzer{completer: completer}
}

const imagePrompt = `你是台灣的營養師兼運動紀錄助理。先判斷這張圖片屬於哪一類：
- "food"：食物或飲料照片。辨識每一樣食物，依台灣常見份量估計份量與營養素。
- "exercise"：運動 App 或智慧手錶的運動紀錄截圖。讀出運動種類、時間（分鐘）、距離（公里）、消耗熱量（大卡）；畫面沒有的填 0。
- "other"：其他圖片。

只輸出一個 JSON 物件，不要任何其他文字：
{"kind":"food|exercise|other",
 "foods":[{"name":"白飯","portion":"1碗(約200g)","calories":280,"protein_g":5,"carbs_g":62,"fat_g":0.6}],
 "advice":"一句 40 字以內、親切實用的飲食建議（繁體中文）",
 "exercise":{"activity":"跑步","duration_min":30,"distance_km":5,"calories":320}}
不是 food 時 foods 給空陣列；不是 exercise 時 exercise 給 null。食物名稱與份量用繁體中文。`

// AnalyzeImage 分析一張圖片；AI 全部失敗時回傳 ErrUnavailable.
func (a *Analyzer) AnalyzeImage(ctx context.Context, img ai.Image) (*ImageAnalysis, error) {
	var parsed *ImageAnalysis
	res := a.completer.Complete(ctx, &ai.Request{
		Prompt: imagePrompt,
		Images: []ai.Image{img},
		JSON:   true,
		Accept: func(text string) bool {
			parsed = parseAnalysis(text)
			return parsed != nil
		},
	})
	if !res.OK() || parsed == nil {
		return nil, ErrUnavailable
	}
	return parsed, nil
}

type rawAnalysis struct {
	Kind     Kind             `json:"kind"`
	Foods    []meal.FoodItem  `json:"foods"`
	Advice   string           `json:"advice"`
	Exercise *ExerciseReading `json:"exercise"`
}

// 合理範圍，超出視為模型亂答.
const (
	maxItemCalories = 3000
	maxGrams        = 500
	maxAdviceRunes  = 80
	maxNameRunes    = 30
	maxDurationMin  = 24 * 60
	maxDistanceKM   = 500
	maxBurnCalories = 10000
)

// parseAnalysis 解析並檢查模型回答；不合格回傳 nil（讓路由換下一個模型）.
func parseAnalysis(text string) *ImageAnalysis {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil
	}
	var raw rawAnalysis
	if json.Unmarshal([]byte(text[start:end+1]), &raw) != nil {
		return nil
	}
	out := &ImageAnalysis{Kind: raw.Kind, Advice: cleanAdvice(raw.Advice)}
	switch raw.Kind {
	case KindFood:
		for i := range raw.Foods {
			if item, ok := cleanFood(&raw.Foods[i]); ok {
				out.Foods = append(out.Foods, item)
				out.Total.Add(&item.Nutrients)
			}
		}
		if len(out.Foods) == 0 {
			return nil
		}
	case KindExercise:
		reading, ok := cleanExercise(raw.Exercise)
		if !ok {
			return nil
		}
		out.Exercise = reading
	case KindOther:
	default:
		return nil
	}
	return out
}

func cleanFood(item *meal.FoodItem) (meal.FoodItem, bool) {
	name := strings.TrimSpace(item.Name)
	if name == "" || utf8.RuneCountInString(name) > maxNameRunes || item.Calories <= 0 || item.Calories > maxItemCalories {
		return meal.FoodItem{}, false
	}
	return meal.FoodItem{
		Name:    name,
		Portion: strings.TrimSpace(item.Portion),
		Nutrients: meal.Nutrients{
			Calories: math.Round(item.Calories),
			ProteinG: grams(item.ProteinG),
			CarbsG:   grams(item.CarbsG),
			FatG:     grams(item.FatG),
		},
	}, true
}

func grams(v float64) float64 {
	if v < 0 || v > maxGrams || math.IsNaN(v) {
		return 0
	}
	return math.Round(v*10) / 10
}

func cleanExercise(r *ExerciseReading) (*ExerciseReading, bool) {
	if r == nil || strings.TrimSpace(r.Activity) == "" {
		return nil, false
	}
	out := &ExerciseReading{
		Activity:    strings.TrimSpace(r.Activity),
		DurationMin: within(r.DurationMin, maxDurationMin),
		DistanceKM:  within(r.DistanceKM, maxDistanceKM),
		Calories:    within(r.Calories, maxBurnCalories),
	}
	if out.DurationMin == 0 && out.Calories == 0 && out.DistanceKM == 0 {
		return nil, false
	}
	return out, true
}

func within(v, limit float64) float64 {
	if v <= 0 || v > limit || math.IsNaN(v) {
		return 0
	}
	return v
}

func cleanAdvice(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxAdviceRunes {
		return ""
	}
	return s
}
