package fitness

import (
	"chatbot-go/internal/storage/database/profile"
	"math"
)

// DefaultWeightKG 沒設定體重時的估算值.
const DefaultWeightKG = 60

// runningKcalPerKgKM 只有距離時的跑步消耗（約 1 大卡/公斤/公里）.
const runningKcalPerKgKM = 1.0

// Burned 估算運動消耗熱量（大卡）.
func Burned(a *Activity, durationMin, distanceKM, weightKG float64) float64 {
	if weightKG <= 0 {
		weightKG = DefaultWeightKG
	}
	if durationMin <= 0 {
		if a.Name == ActivityRunning && distanceKM > 0 {
			return math.Round(runningKcalPerKgKM * weightKG * distanceKM)
		}
		return 0
	}
	return math.Round(a.metFor(distanceKM, durationMin) * weightKG * durationMin / 60)
}

// EstimateDuration 只有距離時估計時間（分鐘），讓紀錄有時間可顯示.
func EstimateDuration(a *Activity, distanceKM float64) float64 {
	pace := map[string]float64{ActivityRunning: 6, ActivityWalking: 11, ActivityCycling: 3} // 分鐘/公里
	if p, ok := pace[a.Name]; ok && distanceKM > 0 {
		return math.Round(distanceKM * p)
	}
	return 0
}

// sedentaryFactor 日常活動係數（運動另計）.
const sedentaryFactor = 1.2

// DailyBase 基礎代謝 × 日常活動（Mifflin-St Jeor）；資料不足時回傳 0.
func DailyBase(p *profile.Profile) float64 {
	if p == nil || p.WeightKG <= 0 || p.HeightCM <= 0 || p.Age <= 0 || p.Sex == "" {
		return 0
	}
	bmr := 10*p.WeightKG + 6.25*p.HeightCM - 5*float64(p.Age)
	if p.Sex == profile.SexMale {
		bmr += 5
	} else {
		bmr -= 161
	}
	return math.Round(bmr * sedentaryFactor)
}
