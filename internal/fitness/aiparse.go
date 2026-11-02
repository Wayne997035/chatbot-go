package fitness

import (
	"chatbot-go/internal/ai"
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Completer AI 生成（*ai.Router 實作）.
type Completer interface {
	Complete(ctx context.Context, req *ai.Request) ai.Result
}

// AIParser 規則看不懂的運動描述交給 AI 解析（結果一律先請使用者確認再存）.
type AIParser struct {
	completer Completer
	timeout   time.Duration
}

const aiParseTimeout = 12 * time.Second

// NewAIParser 建立 AI 解析器；completer 為 nil 時回傳 nil.
func NewAIParser(completer Completer) *AIParser {
	if completer == nil {
		return nil
	}
	return &AIParser{completer: completer, timeout: aiParseTimeout}
}

type aiWorkout struct {
	IsWorkout   bool    `json:"is_workout"`
	Activity    string  `json:"activity"`
	DurationMin float64 `json:"duration_min"`
	DistanceKM  float64 `json:"distance_km"`
	Calories    float64 `json:"calories"`
}

// Parse 回傳解析結果；不是運動或 AI 無法使用時 ok 為 false.
func (p *AIParser) Parse(ctx context.Context, text string) (parsed *Parsed, ok bool) {
	if p == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var result *aiWorkout
	res := p.completer.Complete(ctx, &ai.Request{
		Prompt: "判斷使用者這句話是不是在記錄自己做過的一次運動。是的話整理成數字；不是（例如查地名、閒聊）就回 is_workout=false。\n" +
			"只輸出 JSON：{\"is_workout\":true,\"activity\":\"運動名稱（繁體中文，如跑步、重訓、游泳）\"," +
			"\"duration_min\":分鐘數,\"distance_km\":公里數,\"calories\":使用者有寫的消耗大卡} 沒提到的數字填 0。\n\n" +
			"使用者：" + text,
		JSON: true,
		Accept: func(answer string) bool {
			result = parseAIWorkout(answer)
			return result != nil
		},
	})
	if !res.OK() || result == nil || !result.IsWorkout {
		return nil, false
	}
	out := &Parsed{
		Activity:    ActivityByName(result.Activity),
		DurationMin: within(result.DurationMin, maxDurationMin),
		DistanceKM:  within(result.DistanceKM, maxDistanceKM),
		Calories:    within(result.Calories, maxBurnCalories),
	}
	ok = out.HasAmount()
	return out, ok
}

func parseAIWorkout(text string) *aiWorkout {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil
	}
	var w aiWorkout
	if json.Unmarshal([]byte(text[start:end+1]), &w) != nil {
		return nil
	}
	if w.IsWorkout && strings.TrimSpace(w.Activity) == "" {
		return nil
	}
	return &w
}

// 合理範圍.
const (
	maxDurationMin  = 24 * 60
	maxDistanceKM   = 500
	maxBurnCalories = 10000
)

// Plausible 運動數值是否合理；超過一天、500 公里或 10000 大卡視為打錯，不能存（會讓統計從此失真）.
func Plausible(durationMin, distanceKM, calories float64) bool {
	return durationMin <= maxDurationMin && distanceKM <= maxDistanceKM && calories <= maxBurnCalories
}

func within(v, limit float64) float64 {
	if v <= 0 || v > limit {
		return 0
	}
	return v
}
