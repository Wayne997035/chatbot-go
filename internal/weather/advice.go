package weather

import (
	"chatbot-go/internal/ai"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// Completer AI 文字生成（*ai.Router 實作）.
type Completer interface {
	Complete(ctx context.Context, req *ai.Request) ai.Result
}

// Advisor 依天氣預報產生一句生活建議：先問 AI，失敗或沒設定就用規則判斷.
type Advisor struct {
	completer   Completer
	redisClient *redis.Client
	cacheTTL    time.Duration
	timeout     time.Duration
}

const (
	tipCacheTTL  = 2 * time.Hour
	tipTimeout   = 8 * time.Second
	tipMaxRunes  = 80
	tipKeyPrefix = "wtip:"
)

// NewAdvisor 建立建議產生器；completer 與 redisClient 都可為 nil.
func NewAdvisor(completer Completer, redisClient *redis.Client) *Advisor {
	return &Advisor{completer: completer, redisClient: redisClient, cacheTTL: tipCacheTTL, timeout: tipTimeout}
}

// Tip 回傳一句建議；沒有可說的就回傳空字串. 不會讓天氣回覆失敗.
func (a *Advisor) Tip(ctx context.Context, f *weatherstore.WeatherForecast) string {
	if a == nil || f == nil {
		return ""
	}
	key := tipKeyPrefix + f.City + f.District + ":" + f.CreateTime
	if a.redisClient != nil {
		if cached, err := a.redisClient.Get(ctx, key).Result(); err == nil && cached != "" {
			return cached
		}
	}
	tip := a.aiTip(ctx, f)
	if tip == "" {
		return RuleTip(f)
	}
	if a.redisClient != nil {
		a.redisClient.Set(ctx, key, tip, a.cacheTTL)
	}
	return tip
}

func (a *Advisor) aiTip(ctx context.Context, f *weatherstore.WeatherForecast) string {
	if a.completer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var tip string
	res := a.completer.Complete(ctx, &ai.Request{
		Prompt: tipPrompt(f),
		JSON:   true,
		Accept: func(text string) bool {
			tip = parseTip(text)
			return tip != ""
		},
	})
	if !res.OK() {
		slog.Warn("weather tip AI unavailable, using rule-based tip", "errors", strings.Join(res.Errors, "；"))
		return ""
	}
	return parseTip(res.Text)
}

func tipPrompt(f *weatherstore.WeatherForecast) string {
	var sb strings.Builder
	for _, el := range f.Elements {
		if el.Value != "" {
			fmt.Fprintf(&sb, "%s：%s%s\n", el.Description, el.Value, unitSuffix(el.ElementName))
		}
	}
	return "你是貼心的台灣天氣小幫手。根據以下「" + f.City + f.District + "」的天氣預報，" +
		"用繁體中文給一句 40 字以內的實用建議（穿著、帶傘、防曬、補水或戶外運動擇重點），語氣親切、不要重複數字。\n" +
		"只輸出 JSON：{\"tip\": \"建議內容\"}\n\n" + sb.String()
}

func parseTip(text string) string {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return ""
	}
	var payload struct {
		Tip string `json:"tip"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &payload) != nil {
		return ""
	}
	tip := strings.TrimSpace(payload.Tip)
	if tip == "" || utf8.RuneCountInString(tip) > tipMaxRunes {
		return ""
	}
	return tip
}

// 規則建議的門檻.
const (
	rainyPoP     = 50
	hotApparent  = 33
	coldApparent = 15
)

// RuleTip 不靠 AI 的基本建議.
func RuleTip(f *weatherstore.WeatherForecast) string {
	var tips []string
	if pop, ok := elementNumber(f, "PoP12h", "PoP6h"); ok && pop >= rainyPoP {
		tips = append(tips, "降雨機率偏高，出門記得帶傘")
	}
	if temp, ok := elementNumber(f, "AT", "T"); ok {
		switch {
		case temp >= hotApparent:
			tips = append(tips, "天氣炎熱，注意防曬與補充水分")
		case temp <= coldApparent:
			tips = append(tips, "氣溫偏低，記得多加件外套")
		}
	}
	return strings.Join(tips, "；")
}

// elementNumber 依序找第一個有數值的元素.
func elementNumber(f *weatherstore.WeatherForecast, names ...string) (float64, bool) {
	for _, name := range names {
		for _, el := range f.Elements {
			if el.ElementName != name {
				continue
			}
			if v, err := strconv.ParseFloat(strings.TrimSpace(el.Value), 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}
