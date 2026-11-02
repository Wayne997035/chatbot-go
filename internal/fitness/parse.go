package fitness

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Parsed 從文字解析出的運動；Activity 為 nil 表示沒提到運動種類.
type Parsed struct {
	Activity    *Activity
	DurationMin float64
	DistanceKM  float64
	// Calories 使用者自己寫的消耗熱量（例如手錶顯示的數字）.
	Calories float64
}

// HasAmount 是否有時間、距離或熱量.
func (p *Parsed) HasAmount() bool {
	return p.DurationMin > 0 || p.DistanceKM > 0 || p.Calories > 0
}

var (
	reHourMin  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:個)?\s*(?:小時|鐘頭|hours?|hrs?|h)\s*(?:(\d+)\s*(?:分鐘|分|min))?`)
	reHalfHour = regexp.MustCompile(`(\d+)?\s*個?半\s*(?:小時|鐘頭)`)
	reMinutes  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:分鐘|分|mins?|minutes?)`)
	reKM       = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:公里|(?:km|k)\b)`)
	reMeters   = regexp.MustCompile(`(\d{3,})\s*(?:公尺|米|m\b)`)
	reCalories = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:大卡|千卡|kcal|卡|cal)`)
)

// 中文數字（常見寫法）轉阿拉伯數字，例如「三十分鐘」「一個半小時」「兩小時」.
var chineseDigits = map[rune]int{'零': 0, '一': 1, '二': 2, '兩': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

var reChineseNumber = regexp.MustCompile(`[零一二兩三四五六七八九十]+`)

func replaceChineseNumbers(s string) string {
	return reChineseNumber.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := chineseToInt(m); ok {
			return strconv.Itoa(v)
		}
		return m
	})
}

func chineseToInt(s string) (int, bool) {
	total, current := 0, 0
	for _, r := range s {
		if r == '十' {
			if current == 0 {
				current = 1
			}
			total += current * 10
			current = 0
			continue
		}
		d, ok := chineseDigits[r]
		if !ok {
			return 0, false
		}
		current = current*10 + d
	}
	return total + current, true
}

// normalize 全形轉半形、中文數字轉阿拉伯數字、轉小寫.
func normalize(text string) string {
	var sb strings.Builder
	for _, r := range text {
		switch {
		case r >= '０' && r <= '９':
			r = '0' + (r - '０')
		case r >= 'Ａ' && r <= 'Ｚ':
			r = 'a' + (r - 'Ａ')
		case r >= 'ａ' && r <= 'ｚ':
			r = 'a' + (r - 'ａ')
		case r == '．':
			r = '.'
		}
		sb.WriteRune(r)
	}
	return strings.ToLower(replaceChineseNumbers(sb.String()))
}

// Parse 寬鬆解析運動描述，例如「跑步5公里30分」「今天慢跑半小時」「重訓 1小時20分」「騎車 20k」.
func Parse(text string) *Parsed {
	s := normalize(text)
	p := &Parsed{}

	switch {
	case strings.Contains(s, "全馬"):
		p.DistanceKM = 42.195
	case strings.Contains(s, "半馬"):
		p.DistanceKM = 21.1
	}
	if m := reKM.FindStringSubmatch(s); m != nil {
		p.DistanceKM = atof(m[1])
		s = strings.Replace(s, m[0], " ", 1)
	} else if m := reMeters.FindStringSubmatch(s); m != nil {
		p.DistanceKM = atof(m[1]) / 1000
		s = strings.Replace(s, m[0], " ", 1)
	}
	if m := reCalories.FindStringSubmatch(s); m != nil {
		p.Calories = atof(m[1])
		s = strings.Replace(s, m[0], " ", 1)
	}
	p.DurationMin = parseDuration(s)
	p.Activity = FindActivity(s)
	return p
}

func parseDuration(s string) float64 {
	if m := reHalfHour.FindStringSubmatch(s); m != nil {
		return atof(m[1])*60 + 30
	}
	if m := reHourMin.FindStringSubmatch(s); m != nil {
		minutes := atof(m[1])*60 + atof(m[2])
		if strings.Contains(s, "小時半") || strings.Contains(s, "鐘頭半") {
			minutes += 30
		}
		return minutes
	}
	if m := reMinutes.FindStringSubmatch(s); m != nil {
		return atof(m[1])
	}
	return 0
}

func atof(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// IsActivityOnly 文字只有運動名稱（例如點了「跑步」按鈕），可以接著問時間.
// 單字別名（跑、走、游）不算，避免把含這些字的地名當成運動.
func IsActivityOnly(text string) bool {
	s := strings.TrimPrefix(strings.TrimSpace(normalize(text)), "記")
	if utf8.RuneCountInString(s) < 2 {
		return false
	}
	for _, a := range aliasIndex {
		if s == a.text || strings.EqualFold(s, a.activity.Name) {
			return true
		}
	}
	return false
}

// workoutHints 看起來像在描述運動的字；只有含這些字才交給 AI 解析，避免把一般查詢送去 AI.
var workoutHints = []string{"運動", "練", "跑", "走", "騎", "游", "訓", "球", "操", "瑜", "爬", "跳", "舞", "分鐘", "小時", "公里", "km", "大卡"}

// LooksLikeWorkout 是否值得交給 AI 判斷.
func LooksLikeWorkout(text string) bool {
	return containsAny(normalize(text), workoutHints)
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
