package recordedit

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/workout"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"time"
)

// 運動數值範圍（與 fitness.Plausible 同值；Plausible 只擋上限，下限由這裡擋）.
const (
	maxDurationMin = 24 * 60
	maxDistanceKM  = 500
	maxBurnKcal    = 10000
)

// doneTimeRe 台北時間 HH:MM，兩位數；「7:5」「25:00」「abc」一律拒絕.
var doneTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// MinDoneAt [F33-13] DoneAt 的下限：台北時間今天 00:00 往前 MaxBackdateDays-1 天
// （含今天共 90 天；與 LIFF 統計頁可查範圍 liff.maxDays=90 同值，liff 日後調整需同步）.
func MinDoneAt(now time.Time) time.Time {
	from, _ := nutrition.DayRange(now)
	return from.AddDate(0, 0, -(MaxBackdateDays - 1))
}

// recordVersion 讀紀錄的 Version 欄位；沒有該欄位（workout.Workout.Version 尚未合入）時回 0.
// [F33-13] 用反射是為了讓契約在 E-B 的 Version 欄位合入前後都能編譯，合入後行為自動生效.
func recordVersion(v any) int64 {
	f := reflect.Indirect(reflect.ValueOf(v)).FieldByName("Version")
	if f.IsValid() && f.CanInt() {
		return f.Int()
	}
	return 0
}

// BuildWorkoutProposal [F33-13] 試算運動修改並產生提議；入參 w 不被修改.
// 時長或距離改了而沒給熱量 → fitness.Burned 重算；只改 DoneAt 不重算熱量；DoneAt 不得晚於 now、不得早於 MinDoneAt(now).
func BuildWorkoutProposal(plan *Plan, w *workout.Workout, weightKG float64, now time.Time) (*Proposal, error) {
	if plan == nil || w == nil {
		return nil, invalid("nil plan or workout")
	}
	base := Proposal{Kind: KindWorkout, Op: plan.Op, RecordID: w.ID, BaseVersion: recordVersion(w), Plan: *plan, Before: w.Calories}
	switch plan.Op {
	case OpDeleteWorkout:
		base.After = 0
		base.Summary = fmt.Sprintf("要刪除 %s 的「%s」（%s、%.0f 大卡）嗎？刪除後無法復原。",
			dayLabel(w.DoneAt), SanitizeDisplay(w.Activity, MaxNameRunes), fitness.Describe(w), w.Calories)
		return &base, nil
	case OpSetWorkout:
	default:
		return nil, invalid("op %q is not a workout edit", plan.Op)
	}
	res, err := computeWorkout(plan, w, weightKG, now)
	if err != nil {
		return nil, err
	}
	base.Workout, base.After = res, res.Calories
	base.Summary = workoutSummary(w, res, plan.Basis)
	return &base, nil
}

// ApplyWorkout [F33-13] 把提議的最終值（提議當下算好、確認時不重算）寫進運動；CreateTime 不動；失敗時 w 不被改動.
func (p *Proposal) ApplyWorkout(w *workout.Workout) error {
	if p == nil || w == nil || p.Kind != KindWorkout || p.Op != OpSetWorkout || p.Workout == nil {
		return invalid("not a workout proposal")
	}
	r := p.Workout
	if !validWorkoutValues(r.DurationMin, r.DistanceKM, r.Calories) || r.DoneAt.IsZero() {
		return outOfRange("運動數值不合理")
	}
	w.DurationMin, w.DistanceKM, w.Calories, w.DoneAt = r.DurationMin, r.DistanceKM, r.Calories, r.DoneAt
	return nil
}

func validWorkoutValues(dur, dist, kcal float64) bool {
	for _, v := range []float64{dur, dist, kcal} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return dur <= maxDurationMin && dist <= maxDistanceKM && kcal <= maxBurnKcal && fitness.Plausible(dur, dist, kcal) && (dur > 0 || kcal > 0)
}

// checkWorkoutPlan plan 內的運動數值先過範圍（負值、超上限一律拒絕）.
func checkWorkoutPlan(plan *Plan) error {
	switch {
	case plan.DurationMin < 0 || plan.DurationMin > maxDurationMin:
		return outOfRange("運動時間需在 1–%d 分鐘", maxDurationMin)
	case plan.DistanceKM < 0 || plan.DistanceKM > maxDistanceKM:
		return outOfRange("距離需在 0–%d 公里", maxDistanceKM)
	case plan.Kcal < 0 || plan.Kcal > maxBurnKcal:
		return outOfRange("熱量需在 1–%d 大卡", maxBurnKcal)
	}
	return nil
}

func computeWorkout(plan *Plan, w *workout.Workout, weightKG float64, now time.Time) (*WorkoutResult, error) {
	res := &WorkoutResult{DurationMin: w.DurationMin, DistanceKM: w.DistanceKM, Calories: w.Calories, DoneAt: w.DoneAt}
	if err := checkWorkoutPlan(plan); err != nil {
		return nil, err
	}
	if plan.DurationMin > 0 {
		res.DurationMin = math.Round(plan.DurationMin)
	}
	if plan.DistanceKM > 0 {
		res.DistanceKM = math.Round(plan.DistanceKM*10) / 10
	}
	changedLoad := res.DurationMin != w.DurationMin || res.DistanceKM != w.DistanceKM
	switch {
	case plan.Kcal > 0:
		res.Calories = math.Round(plan.Kcal) // 客人給了熱量：採用，不重算.
		if res.Calories < 1 {
			return nil, outOfRange("熱量需在 1–%d 大卡", maxBurnKcal)
		}
	case changedLoad:
		res.Calories = fitness.Burned(fitness.ActivityByName(w.Activity), res.DurationMin, res.DistanceKM, weightKG)
		if res.Calories <= 0 {
			return nil, outOfRange("依這個時間與距離算不出消耗熱量")
		}
	}
	doneAt, err := newDoneAt(plan, w.DoneAt, now)
	if err != nil {
		return nil, err
	}
	res.DoneAt = doneAt
	if !validWorkoutValues(res.DurationMin, res.DistanceKM, res.Calories) {
		return nil, outOfRange("運動數值不合理")
	}
	if res.DurationMin == w.DurationMin && res.DistanceKM == w.DistanceKM && res.Calories == w.Calories && res.DoneAt.Equal(w.DoneAt) {
		return nil, ErrNoChange
	}
	return res, nil
}

// newDoneAt 在台北時區組出新的 DoneAt：沒給日期偏移沿用原日期、沒給時刻沿用原時刻；
// 結果不得晚於 now、不得早於 MinDoneAt(now).
func newDoneAt(plan *Plan, old, now time.Time) (time.Time, error) {
	if plan.DoneDayOffset == nil && plan.DoneTime == "" {
		return old, nil
	}
	loc := nutrition.Taipei
	local := old.In(loc)
	y, mo, d := local.Date()
	h, mi, s := local.Clock()
	if off := plan.DoneDayOffset; off != nil {
		if *off > 0 { // [F33-38] 客人說明天、後天：模型照實填正數，這裡擋下並說明原因（決策 91563b18 ③）.
			return time.Time{}, outOfRange("發生時間不能晚於現在")
		}
		if *off < -(MaxBackdateDays - 1) {
			return time.Time{}, outOfRange("日期只能改到今天以前 %d 天內", MaxBackdateDays-1)
		}
		ty, tm, td := now.In(loc).Date()
		y, mo, d = time.Date(ty, tm, td+*off, 0, 0, 0, 0, loc).Date()
	}
	if plan.DoneTime != "" {
		m := doneTimeRe.FindStringSubmatch(plan.DoneTime)
		if m == nil {
			return time.Time{}, outOfRange("時間格式需為 HH:MM（例如 19:00）")
		}
		h, _ = strconv.Atoi(m[1])
		mi, _ = strconv.Atoi(m[2])
		s = 0
	}
	at := time.Date(y, mo, d, h, mi, s, 0, loc)
	if at.After(now) {
		return time.Time{}, outOfRange("發生時間不能晚於現在")
	}
	if at.Before(MinDoneAt(now)) {
		return time.Time{}, outOfRange("發生時間最多只能改到 %d 天內", MaxBackdateDays)
	}
	return at, nil
}

func dayLabel(t time.Time) string { return t.In(nutrition.Taipei).Format("01/02") }

func dayTimeLabel(t time.Time) string { return t.In(nutrition.Taipei).Format("01/02 15:04") }

// workoutSummary 確認卡文字：改了哪些欄位就寫哪些，日期時刻必寫出.
func workoutSummary(old *workout.Workout, res *WorkoutResult, basis string) string {
	name := SanitizeDisplay(old.Activity, MaxNameRunes)
	var out string
	if res.DurationMin != old.DurationMin || res.DistanceKM != old.DistanceKM || res.Calories != old.Calories {
		out = fmt.Sprintf("要把 %s 的「%s」從 %s／%.0f 大卡 改成 %s／%.0f 大卡嗎？", dayLabel(old.DoneAt), name,
			loadLabel(old.DurationMin, old.DistanceKM), old.Calories, loadLabel(res.DurationMin, res.DistanceKM), res.Calories)
		if !res.DoneAt.Equal(old.DoneAt) {
			out += fmt.Sprintf("\n發生時間也從 %s 改成 %s", dayTimeLabel(old.DoneAt), dayTimeLabel(res.DoneAt))
		}
	} else {
		out = fmt.Sprintf("要把 %s 的「%s」的時間從 %s 改成 %s 嗎？", dayLabel(old.DoneAt), name, dayTimeLabel(old.DoneAt), dayTimeLabel(res.DoneAt))
	}
	return out + basisLine(basis)
}

func loadLabel(durationMin, distanceKM float64) string {
	s := fmt.Sprintf("%.0f 分鐘", durationMin)
	if distanceKM > 0 {
		s += fmt.Sprintf("／%g 公里", distanceKM)
	}
	return s
}
