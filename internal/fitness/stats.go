package fitness

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"fmt"
	"math"
	"strings"
	"time"
)

// DayStat 一天的熱量收支.
type DayStat struct {
	Date     time.Time
	Intake   float64
	Meals    int
	Burned   float64
	Workouts int
	RunKM    float64
}

// Summarize 依日期彙整；days 為由舊到新的每日起點.
func Summarize(days []time.Time, meals []meal.Meal, workouts []workout.Workout) []DayStat {
	stats := make([]DayStat, len(days))
	for i, d := range days {
		stats[i].Date = d
	}
	index := func(t time.Time) int {
		for i := len(days) - 1; i >= 0; i-- {
			if !t.Before(days[i]) {
				return i
			}
		}
		return -1
	}
	for i := range meals {
		if j := index(meals[i].EatenAt); j >= 0 {
			stats[j].Intake += meals[i].Total.Calories
			stats[j].Meals++
		}
	}
	for i := range workouts {
		w := &workouts[i]
		if j := index(w.DoneAt); j >= 0 {
			stats[j].Burned += w.Calories
			stats[j].Workouts++
			if w.Activity == ActivityRunning {
				stats[j].RunKM += w.DistanceKM
			}
		}
	}
	return stats
}

// Describe 一筆運動的簡短描述，例如「跑步 30 分鐘・5 公里」.
func Describe(w *workout.Workout) string {
	parts := []string{w.Activity}
	if w.DurationMin > 0 {
		parts = append(parts, fmt.Sprintf("%s 分鐘", num(w.DurationMin)))
	}
	desc := strings.Join(parts, " ")
	if w.DistanceKM > 0 {
		desc += fmt.Sprintf("・%s 公里", num(w.DistanceKM))
	}
	return desc
}

// FormatRecorded 記錄成功的回覆.
func FormatRecorded(w *workout.Workout, weightKG, todayBurned float64, fromApp bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ 已記錄：%s\n", Describe(w))
	switch {
	case fromApp:
		fmt.Fprintf(&sb, "消耗 %s 大卡（依運動 App 數據）", num(w.Calories))
	case weightKG > 0:
		fmt.Fprintf(&sb, "消耗約 %s 大卡（以體重 %s 公斤計算）", num(w.Calories), num(weightKG))
	default:
		fmt.Fprintf(&sb, "消耗約 %s 大卡（以 %d 公斤估算，輸入「體重 70」可更準確）", num(w.Calories), DefaultWeightKG)
	}
	fmt.Fprintf(&sb, "\n今日運動共消耗 %s 大卡", num(todayBurned))
	return sb.String()
}

// FormatToday 今日熱量收支；base 為基礎代謝＋日常消耗，0 表示身體資料不足.
func FormatToday(meals []meal.Meal, workouts []workout.Workout, base float64) string {
	var sb strings.Builder
	intake := meal.Sum(meals).Calories
	var burned float64
	descs := make([]string, 0, len(workouts))
	for i := range workouts {
		burned += workouts[i].Calories
		descs = append(descs, Describe(&workouts[i]))
	}
	sb.WriteString("📊 今日熱量收支\n")
	fmt.Fprintf(&sb, "攝取：%s 大卡（%d 餐）\n", num(intake), len(meals))
	if len(descs) > 0 {
		fmt.Fprintf(&sb, "運動消耗：%s 大卡（%s）\n", num(burned), strings.Join(descs, "、"))
	} else {
		sb.WriteString("運動消耗：0 大卡（今天還沒運動）\n")
	}
	if base > 0 {
		net := intake - base - burned
		fmt.Fprintf(&sb, "基礎代謝＋日常：%s 大卡\n淨值：%s 大卡（%s）", num(base), signed(net), balanceLabel(net))
	} else {
		fmt.Fprintf(&sb, "攝取 − 運動：%s 大卡\n設定「體重 70 身高 170 30歲 男」可計算基礎代謝與熱量赤字", signed(intake-burned))
	}
	return sb.String()
}

// FormatWeek 最近七天統計.
func FormatWeek(stats []DayStat) string {
	var sb strings.Builder
	var intake, burned, runKM float64
	var workouts int
	sb.WriteString("📅 近 7 天統計\n")
	for i := range stats {
		d := &stats[i]
		intake += d.Intake
		burned += d.Burned
		runKM += d.RunKM
		workouts += d.Workouts
		fmt.Fprintf(&sb, "%s 攝取 %s｜運動 %s\n", d.Date.Format("01/02"), num(d.Intake), num(d.Burned))
	}
	fmt.Fprintf(&sb, "合計：攝取 %s 大卡、運動消耗 %s 大卡（%d 次）", num(intake), num(burned), workouts)
	if runKM > 0 {
		fmt.Fprintf(&sb, "\n跑步總距離：%s 公里", num(runKM))
	}
	if n := float64(len(stats)); n > 0 {
		fmt.Fprintf(&sb, "\n每日平均攝取 %s 大卡", num(intake/n))
	}
	return sb.String()
}

func balanceLabel(net float64) string {
	if net < 0 {
		return "熱量赤字"
	}
	return "熱量盈餘"
}

func signed(v float64) string {
	if v > 0 {
		return "+" + num(v)
	}
	return num(v)
}

// num 整數不顯示小數，其他保留一位.
func num(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}
