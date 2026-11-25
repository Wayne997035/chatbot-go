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
	reHalfHour = regexp.MustCompile(`(\d+)?\s*個?半\s*個?\s*(?:小時|鐘頭)`) // [F36-07] 含「半個小時」「半個鐘頭」
	reMinutes  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:分鐘|分|mins?|minutes?)`)
	reKM       = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:公里|(?:km|k)\b)`)
	reMeters   = regexp.MustCompile(`(\d{3,})\s*(?:公尺|米|m\b)`)
	reCalories = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:大卡|千卡|kcal|卡|cal)`)
)

// 中文數字（常見寫法）轉阿拉伯數字，例如「三十分鐘」「一個半小時」「兩小時」.
var chineseDigits = map[rune]int{'零': 0, '一': 1, '二': 2, '兩': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

var reChineseNumber = regexp.MustCompile(`[零一二兩三四五六七八九十百千]+`)

// reChineseDecimal 中文小數「二點五小時」「十二點五公里」；只在後面緊接 公里/小時/鐘頭 時才轉，
// 避免把「下午三點」這種時刻當成小數.
var reChineseDecimal = regexp.MustCompile(`([零一二兩三四五六七八九十百千]+)點([零一二兩三四五六七八九]+)((?:個)?(?:公里|小時|鐘頭))`)

// replaceChineseNumbers 把中文數字換成阿拉伯數字；模稜兩可的寫法（一百五）原樣保留，讓後面的規則解析不出來.
// [F36-07] fail-closed：寧可請使用者重打，也不靜默存錯數值（2dbeafd6）.
func replaceChineseNumbers(s string) string {
	s = reChineseDecimal.ReplaceAllStringFunc(s, func(m string) string {
		sub := reChineseDecimal.FindStringSubmatch(m)
		whole, ok := chineseToInt(sub[1])
		if !ok {
			return m
		}
		var frac strings.Builder
		for _, r := range sub[2] {
			frac.WriteString(strconv.Itoa(chineseDigits[r]))
		}
		return strconv.Itoa(whole) + "." + frac.String() + sub[3]
	})
	return reChineseNumber.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := chineseToInt(m); ok {
			return strconv.Itoa(v)
		}
		return m
	})
}

// chineseToInt 轉換一段中文數字；百/千 前面必須有數字，且最後一個 百/千 後面若只剩單一位數
// （一百五、一千二）無法判斷是 105 還是 150，回 false.
func chineseToInt(s string) (int, bool) {
	runes := []rune(s)
	lastUnit := -1
	for i, r := range runes {
		if r == '百' || r == '千' {
			lastUnit = i
		}
	}
	if lastUnit >= 0 && len(runes)-lastUnit-1 == 1 {
		if d, ok := chineseDigits[runes[lastUnit+1]]; ok && d != 0 {
			return 0, false
		}
	}
	total, current := 0, 0
	for _, r := range runes {
		switch r {
		case '十':
			if current == 0 {
				current = 1
			}
			total += current * 10
			current = 0
		case '百', '千':
			if current == 0 {
				return 0, false
			}
			unit := 100
			if r == '千' {
				unit = 1000
			}
			total += current * unit
			current = 0
		default:
			d, ok := chineseDigits[r]
			if !ok {
				return 0, false
			}
			current = current*10 + d
		}
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

// extraSportNames 不在 aliasIndex、但字面就是在講運動的詞（[F37-01]）.
// 單字「練」「騎」不算（會撞到練琴、騎樓）；多字別名「騎車」「騎腳踏車」「健身」「重訓」已在 aliasIndex.
var extraSportNames = []string{"運動", "訓練"}

// sportVerbObject 動詞與球/船名被數量或助詞隔開的說法：「打了一場球」「划了20分鐘船」（視窗以字元計）.
var sportVerbObject = regexp.MustCompile(`打.{0,4}球|划.{0,6}船`)

// HasSportName [F37-01] 文字含運動名：aliasIndex 內 2 字以上的別名、單字「跑」、extraSportNames、或打..球 / 划..船.
// 單字「走」「游」不算（路好難走、游先生）；時間、距離、熱量字也不算，這類閒聊改走意圖 / 問答.
func HasSportName(text string) bool {
	s := normalize(text)
	for _, a := range aliasIndex {
		if utf8.RuneCountInString(a.text) >= 2 && strings.Contains(s, a.text) {
			return true
		}
	}
	return strings.Contains(s, "跑") || containsAny(s, extraSportNames) || sportVerbObject.MatchString(s)
}

// LooksLikeWorkout 句子是否像在描述運動（決定回記錄教學句，或讓記餐改走運動）；要有運動名才算 [F37-01].
func LooksLikeWorkout(text string) bool {
	return HasSportName(text)
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
