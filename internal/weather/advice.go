package weather

import (
	weatherstore "chatbot-go/internal/storage/database/weather"
	"context"
	"strconv"
	"strings"
)

// Advisor [F36-06] 依天氣預報產生一句生活建議：只用規則（決策 b8a19dfd ①），不呼叫 AI.
// 以前先問 AI 再退規則，每則天氣回覆多花一次 AI 額度；拔掉後 advice.go 不得再 import internal/ai.
type Advisor struct{}

// NewAdvisor 建立建議產生器.
func NewAdvisor() *Advisor { return &Advisor{} }

// Tip 回傳一句建議（內容 = RuleTip）；沒有可說的就回傳空字串. nil 安全，不會讓天氣回覆失敗.
func (a *Advisor) Tip(_ context.Context, f *weatherstore.WeatherForecast) string {
	if a == nil || f == nil {
		return ""
	}
	return RuleTip(f)
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
	pop, popOK := elementNumber(f, "PoP12h", "PoP6h")
	if popOK && pop >= rainyPoP {
		tips = append(tips, "降雨機率偏高，出門記得帶傘")
	}
	temp, tempOK := elementNumber(f, "AT", "T")
	if tempOK {
		switch {
		case temp >= hotApparent:
			tips = append(tips, "天氣炎熱，注意防曬與補充水分")
		case temp <= coldApparent:
			tips = append(tips, "氣溫偏低，記得多加件外套")
		}
	}
	// [F36-08] 三條規則都沒命中、且降雨機率與體感/氣溫都讀得到才補舒適句；缺資料時不能宣稱舒適（5f54bc90）.
	if len(tips) == 0 && popOK && tempOK {
		return "天氣舒適，適合外出走走"
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
