package fitness

import (
	"chatbot-go/internal/storage/database/profile"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ProfileUpdate 從文字解析出的身體資料；0 / 空字串表示這次沒提到.
type ProfileUpdate struct {
	WeightKG float64
	HeightCM float64
	Age      int
	Sex      string
}

var (
	reWeight      = regexp.MustCompile(`體重\D{0,3}?(\d{2,3}(?:\.\d+)?)`)
	reWeightUnit  = regexp.MustCompile(`(\d{2,3}(?:\.\d+)?)\s*(?:公斤|kg)`)
	reHeight      = regexp.MustCompile(`身高\D{0,3}?(\d{3}(?:\.\d+)?)`)
	reHeightUnit  = regexp.MustCompile(`(\d{3}(?:\.\d+)?)\s*(?:公分|cm)`)
	reAge         = regexp.MustCompile(`(\d{1,3})\s*歲`)
	reAgeKeyword  = regexp.MustCompile(`年齡\D{0,3}?(\d{1,3})`)
	profileTokens = []string{"體重", "身高", "歲", "年齡", "性別", "公斤", "kg", "公分", "cm"}
)

// 合理範圍.
const (
	minWeight, maxWeight = 25, 300
	minHeight, maxHeight = 100, 250
	minAge, maxAge       = 10, 100
)

// ParseProfile 解析「體重 70 身高 170 30歲 男」之類的設定；沒有可用欄位時 ok 為 false.
func ParseProfile(text string) (ProfileUpdate, bool) {
	s := normalize(text)
	if !containsAny(s, profileTokens) {
		return ProfileUpdate{}, false
	}
	var u ProfileUpdate
	if v := firstNumber(s, reWeight, reWeightUnit); v >= minWeight && v <= maxWeight {
		u.WeightKG = v
	}
	if v := firstNumber(s, reHeight, reHeightUnit); v >= minHeight && v <= maxHeight {
		u.HeightCM = v
	}
	if v := int(firstNumber(s, reAge, reAgeKeyword)); v >= minAge && v <= maxAge {
		u.Age = v
	}
	switch {
	case strings.Contains(s, "男"):
		u.Sex = profile.SexMale
	case strings.Contains(s, "女"):
		u.Sex = profile.SexFemale
	}
	return u, u.WeightKG > 0 || u.HeightCM > 0 || u.Age > 0 || u.Sex != ""
}

func firstNumber(s string, patterns ...*regexp.Regexp) float64 {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(s); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// Apply 把這次提到的欄位套用到既有資料.
func (u *ProfileUpdate) Apply(p *profile.Profile) {
	if u.WeightKG > 0 {
		p.WeightKG = u.WeightKG
	}
	if u.HeightCM > 0 {
		p.HeightCM = u.HeightCM
	}
	if u.Age > 0 {
		p.Age = u.Age
	}
	if u.Sex != "" {
		p.Sex = u.Sex
	}
}

// FormatProfile 顯示身體資料與還缺哪些欄位.
func FormatProfile(p *profile.Profile) string {
	if p == nil {
		p = &profile.Profile{}
	}
	value := func(v float64, unit string) string {
		if v <= 0 {
			return "未設定"
		}
		return num(v) + unit
	}
	sex := map[string]string{profile.SexMale: "男", profile.SexFemale: "女"}[p.Sex]
	if sex == "" {
		sex = "未設定"
	}
	text := fmt.Sprintf("👤 身體資料\n體重：%s\n身高：%s\n年齡：%s\n性別：%s",
		value(p.WeightKG, " 公斤"), value(p.HeightCM, " 公分"), value(float64(p.Age), " 歲"), sex)
	if base := DailyBase(p); base > 0 {
		text += fmt.Sprintf("\n每日基礎代謝＋日常消耗約 %s 大卡", num(base))
	} else {
		text += "\n輸入「體重 70 身高 170 30歲 男」一次設定，或只改其中一項"
	}
	return text
}

// ErrInvalidProfile 身體資料超出合理範圍.
var ErrInvalidProfile = errors.New("身體資料超出合理範圍")

// ValidateProfile 檢查身體資料；0 / 空字串視為未設定.
func ValidateProfile(p *profile.Profile) error {
	switch {
	case p.WeightKG != 0 && (p.WeightKG < minWeight || p.WeightKG > maxWeight),
		p.HeightCM != 0 && (p.HeightCM < minHeight || p.HeightCM > maxHeight),
		p.Age != 0 && (p.Age < minAge || p.Age > maxAge),
		p.Sex != "" && p.Sex != profile.SexMale && p.Sex != profile.SexFemale:
		return ErrInvalidProfile
	}
	return nil
}
