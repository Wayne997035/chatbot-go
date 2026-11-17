package recordedit

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/workout"
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 17, 10, 0, 0, 0, nutrition.Taipei)

func run20() *workout.Workout {
	return &workout.Workout{
		ID: "w-1", UserID: "U1", Activity: "跑步", DurationMin: 20, DistanceKM: 6, Calories: 314,
		DoneAt:     time.Date(2026, 11, 17, 7, 30, 0, 0, nutrition.Taipei),
		CreateTime: time.Date(2026, 11, 17, 7, 40, 0, 0, nutrition.Taipei),
	}
}

func intp(v int) *int { return &v }

// [F33-13] A7 時長 20→30 沒給 kcal：熱量由 fitness.Burned 重算；給了 kcal 採用不重算.
func TestSetWorkoutRecomputesCalories(t *testing.T) {
	w := run20()
	p, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, Target: "W1", DurationMin: 30}, w, 60, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := fitness.Burned(fitness.ActivityByName("跑步"), 30, 6, 60)
	if want <= 0 || p.Workout.Calories != want || p.After != want || p.Workout.DurationMin != 30 || p.Workout.DistanceKM != 6 {
		t.Errorf("workout = %+v after=%v, want calories %v", p.Workout, p.After, want)
	}
	if w.DurationMin != 20 || w.Calories != 314 {
		t.Fatal("Build must not modify its input")
	}
	if !strings.Contains(p.Summary, "20 分鐘") || !strings.Contains(p.Summary, "30 分鐘") || !strings.Contains(p.Summary, "314") {
		t.Errorf("summary = %q", p.Summary)
	}
	adopted, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, Target: "W1", DurationMin: 30, Kcal: 500}, run20(), 60, testNow)
	if err != nil || adopted.Workout.Calories != 500 {
		t.Errorf("given kcal must be adopted: %+v err=%v", adopted, err)
	}
	distOnly, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, Target: "W1", DistanceKM: 8}, run20(), 60, testNow)
	if err != nil || distOnly.Workout.Calories != fitness.Burned(fitness.ActivityByName("跑步"), 20, 8, 60) {
		t.Errorf("distance change must recompute: %+v err=%v", distOnly, err)
	}
}

// [F33-13] A7 反向：時長 1441、距離 501、kcal 10001、負值 → 拒絕；重算 ≤ 0 拒絕.
func TestSetWorkoutRejectsImplausible(t *testing.T) {
	for _, p := range []Plan{
		{DurationMin: 1441}, {DistanceKM: 501}, {Kcal: 10001}, {DurationMin: -5}, {Kcal: -1}, {DistanceKM: -2}, {Kcal: 0.2},
	} {
		p.Op = OpSetWorkout
		if _, err := BuildWorkoutProposal(&p, run20(), 60, testNow); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%+v: err=%v, want ErrOutOfRange", p, err)
		}
	}
	// 重算 ≤0：沒有時長、沒有距離的非跑步運動，改距離也算不出熱量.
	yoga := &workout.Workout{ID: "w-2", Activity: "瑜珈", Calories: 100, DoneAt: testNow.Add(-time.Hour)}
	if _, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DistanceKM: 3}, yoga, 60, testNow); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("recompute <= 0 must be rejected, got %v", err)
	}
	if _, err := BuildWorkoutProposal(&Plan{Op: OpSetItemKcal}, run20(), 60, testNow); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("meal op on a workout: %v", err)
	}
}

// [F33-13] A34 改發生時間：昨天 19:00；熱量時長不變、CreateTime 不變、確認卡含 11/16 19:00.
func TestSetWorkoutDoneAt(t *testing.T) {
	w := run20()
	p, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, Target: "W1", DoneDayOffset: intp(-1), DoneTime: "19:00"}, w, 60, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wantAt := time.Date(2026, 11, 16, 19, 0, 0, 0, nutrition.Taipei)
	if !p.Workout.DoneAt.Equal(wantAt) || p.Workout.Calories != 314 || p.Workout.DurationMin != 20 || p.After != 314 {
		t.Errorf("result = %+v", p.Workout)
	}
	if !strings.Contains(p.Summary, "11/16 19:00") {
		t.Errorf("summary must show new date and time: %q", p.Summary)
	}
	if err := p.ApplyWorkout(w); err != nil || !w.DoneAt.Equal(wantAt) || w.Calories != 314 || w.CreateTime.Hour() != 7 || w.CreateTime.Minute() != 40 {
		t.Errorf("apply: err=%v w=%+v", err, w)
	}
	// 只給時刻沿用原日期、只給日期偏移沿用原時刻.
	timeOnly, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DoneTime: "06:15"}, run20(), 60, testNow)
	if err != nil || !timeOnly.Workout.DoneAt.Equal(time.Date(2026, 11, 17, 6, 15, 0, 0, nutrition.Taipei)) {
		t.Errorf("time only: %+v err=%v", timeOnly, err)
	}
	dayOnly, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DoneDayOffset: intp(-2)}, run20(), 60, testNow)
	if err != nil || !dayOnly.Workout.DoneAt.Equal(time.Date(2026, 11, 15, 7, 30, 0, 0, nutrition.Taipei)) {
		t.Errorf("day only: %+v err=%v", dayOnly, err)
	}
}

// [F33-13] A34 反向：未來（今天 10:01、+1 天）、壞格式、下限 −89 通過 / −90 拒絕.
func TestSetWorkoutDoneAtRejectsFutureAndTooOld(t *testing.T) {
	bad := []Plan{
		{DoneDayOffset: intp(0), DoneTime: "10:01"},
		{DoneDayOffset: intp(1), DoneTime: "07:00"},
		{DoneTime: "25:00"},
		{DoneTime: "7:5"},
		{DoneTime: "abc"},
		{DoneTime: "12:60"},
		{DoneTime: "7:30"},
		{DoneDayOffset: intp(-90), DoneTime: "12:00"},
		{DoneDayOffset: intp(-365)},
	}
	for _, p := range bad {
		p.Op = OpSetWorkout
		if _, err := BuildWorkoutProposal(&p, run20(), 60, testNow); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%+v: err=%v, want ErrOutOfRange", p, err)
		}
	}
	for _, p := range []Plan{
		{DoneDayOffset: intp(0), DoneTime: "10:00"},
		{DoneDayOffset: intp(-89), DoneTime: "00:00"},
		{DoneDayOffset: intp(-89), DoneTime: "23:59"},
	} {
		p.Op = OpSetWorkout
		if _, err := BuildWorkoutProposal(&p, run20(), 60, testNow); err != nil {
			t.Errorf("%+v must be accepted: %v", p, err)
		}
	}
}

// [F33-38] 決策 91563b18 ③：客人說明天、後天時模型填正數偏移，拒絕原因要說「不能晚於現在」，不是「只能改到 89 天內」.
func TestSetWorkoutFutureDayOffsetSaysNotLaterThanNow(t *testing.T) {
	for _, off := range []int{1, 2} {
		_, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DoneDayOffset: intp(off), DoneTime: "08:00"}, run20(), 60, testNow)
		if !errors.Is(err, ErrOutOfRange) || !strings.Contains(err.Error(), "發生時間不能晚於現在") {
			t.Errorf("offset %d: err=%v, want ErrOutOfRange saying 發生時間不能晚於現在", off, err)
		}
	}
}

// [F33-13] A34 MinDoneAt 以台北午夜計：UTC 16:00 前後的 now 差一天（常數 90 與 liff.maxDays=90 同值）.
func TestMinDoneAtBoundary(t *testing.T) {
	if MaxBackdateDays != 90 {
		t.Fatalf("MaxBackdateDays = %d; liff maxDays is 90 (internal/liff/handler.go) and the two must move together", MaxBackdateDays)
	}
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 11, 17, 15, 59, 0, 0, time.UTC), time.Date(2026, 8, 20, 0, 0, 0, 0, nutrition.Taipei)}, // 台北 11/17 23:59
		{time.Date(2026, 11, 17, 16, 1, 0, 0, time.UTC), time.Date(2026, 8, 21, 0, 0, 0, 0, nutrition.Taipei)},  // 台北 11/18 00:01
		{time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC), time.Date(2026, 8, 21, 0, 0, 0, 0, nutrition.Taipei)},  // 台北 11/18 00:00
	}
	for _, c := range cases {
		if got := MinDoneAt(c.now); !got.Equal(c.want) {
			t.Errorf("MinDoneAt(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	// 邊界：now 在 UTC 16:01（台北 11/18 00:01）時，−89 天 = 8/21 00:00 通過，8/20 不能用 −90 取得.
	now := time.Date(2026, 11, 17, 16, 1, 0, 0, time.UTC)
	if _, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DoneDayOffset: intp(-89), DoneTime: "00:00"}, run20(), 60, now); err != nil {
		t.Errorf("-89 at boundary: %v", err)
	}
	if _, err := BuildWorkoutProposal(&Plan{Op: OpSetWorkout, DoneDayOffset: intp(-90), DoneTime: "23:59"}, run20(), 60, now); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("-90 at boundary: %v", err)
	}
}

// [F33-13] A34 整個 plan 沒改任何欄位 → ErrNoChange（回「本來就是這樣」）.
func TestSetWorkoutNoChangeRejected(t *testing.T) {
	for _, p := range []Plan{
		{Op: OpSetWorkout},
		{Op: OpSetWorkout, DurationMin: 20, DistanceKM: 6, Kcal: 314},
		{Op: OpSetWorkout, DoneDayOffset: intp(0), DoneTime: "07:30"},
	} {
		if _, err := BuildWorkoutProposal(&p, run20(), 60, testNow); !errors.Is(err, ErrNoChange) {
			t.Errorf("%+v: err=%v, want ErrNoChange", p, err)
		}
	}
}

// [F33-13] ApplyWorkout 把 WorkoutResult 的時長、距離、熱量、DoneAt 寫進去且 CreateTime 不變；失敗時不改動.
func TestApplyWorkoutWritesFinalValues(t *testing.T) {
	w := run20()
	created := w.CreateTime
	at := time.Date(2026, 11, 16, 19, 0, 0, 0, nutrition.Taipei)
	p := &Proposal{Kind: KindWorkout, Op: OpSetWorkout, Workout: &WorkoutResult{DurationMin: 45, DistanceKM: 7.5, Calories: 400, DoneAt: at}}
	if err := p.ApplyWorkout(w); err != nil {
		t.Fatal(err)
	}
	if w.DurationMin != 45 || w.DistanceKM != 7.5 || w.Calories != 400 || !w.DoneAt.Equal(at) || !w.CreateTime.Equal(created) {
		t.Errorf("workout = %+v", w)
	}
	for _, bad := range []*Proposal{
		{Kind: KindWorkout, Op: OpSetWorkout},
		{Kind: KindMeal, Op: OpSetWorkout, Workout: &WorkoutResult{DurationMin: 1, Calories: 1, DoneAt: at}},
		{Kind: KindWorkout, Op: OpSetWorkout, Workout: &WorkoutResult{DurationMin: 99999, Calories: 1, DoneAt: at}},
		{Kind: KindWorkout, Op: OpSetWorkout, Workout: &WorkoutResult{DurationMin: 10, Calories: 10}},
	} {
		w2 := run20()
		if err := bad.ApplyWorkout(w2); err == nil || w2.DurationMin != 20 || w2.Calories != 314 {
			t.Errorf("bad proposal %+v: err=%v, workout %+v must be untouched", bad, err, w2)
		}
	}
}

// [F33-13] delete_workout 提議帶被刪紀錄熱量；recordVersion 讀得到 Version 欄位、沒有欄位時為 0.
func TestDeleteWorkoutProposalAndVersion(t *testing.T) {
	p, err := BuildWorkoutProposal(&Plan{Op: OpDeleteWorkout, Target: "W1"}, run20(), 60, testNow)
	if err != nil || p.Before != 314 || p.After != 0 || p.RecordID != "w-1" || !strings.Contains(p.Summary, "11/17") {
		t.Errorf("delete: %+v err=%v", p, err)
	}
	type versioned struct{ Version int64 }
	if got := recordVersion(&versioned{Version: 7}); got != 7 {
		t.Errorf("recordVersion with field = %d, want 7", got)
	}
	if got := recordVersion(&struct{ X int }{}); got != 0 {
		t.Errorf("recordVersion without field = %d, want 0", got)
	}
}
