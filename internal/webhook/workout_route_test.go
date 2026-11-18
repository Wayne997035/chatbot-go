package webhook

import (
	"chatbot-go/internal/fitness"
	"testing"
	"time"
)

// [F34-61] 聊天改紀錄已啟用、近 7 天有可改紀錄時，沒有修改動詞的運動整句仍只記一筆運動，規劃器 0 次.
func TestWorkoutSentenceWithoutSpacesWithRecordEditOn(t *testing.T) {
	const sentence = "跑步6公里20分鐘"
	f := newEditFixture(t)
	f.seedMeal(t, "U1", "麵包", 150, time.Now().Add(-time.Minute)) // 使 hasEditableRecords 為 true
	if !f.h.hasEditableRecords(t.Context(), "U1") {
		t.Fatal("positive control: the seeded meal must make hasEditableRecords true")
	}
	f.plan.answers = []string{`{"op":"set_workout","target":"W1","duration_min":30,"basis":"x"}`}

	reply := f.sayAs(t, "U1", sentence)

	if len(f.workouts.Items) != 1 {
		t.Fatalf("exactly one workout must be written, got %+v", f.workouts.Items)
	}
	w := f.workouts.Items[0]
	wantCal := fitness.Burned(fitness.ActivityByName("跑步"), 20, 6, fitness.DefaultWeightKG)
	if w.Activity != "跑步" || w.DurationMin != 20 || w.DistanceKM != 6 || w.Calories != wantCal {
		t.Errorf("workout = %+v, want 跑步 20 min 6 km %v kcal", w, wantCal)
	}
	if n := f.planner.calls.Load(); n != 0 {
		t.Errorf("planner calls = %d, want 0", n)
	}
	if f.store.Peek("U1") != nil {
		t.Error("no pending edit card may exist")
	}
	mustContain(t, reply, "已記錄")
}

// [F34-61] 含修改動詞的整句進規劃器 1 次、出確認卡，不直接寫成新運動（對照 8d 沒有修改動詞的句子）.
func TestWorkoutSentenceWithEditVerbReachesPlanner(t *testing.T) {
	f := newEditFixture(t)
	f.seedWorkout(t, time.Now().Add(-time.Minute))
	f.plan.answers = []string{planWorkout30}

	reply := f.sayAs(t, "U1", "跑步6公里20分鐘改成30分鐘")

	if n := f.planner.calls.Load(); n != 1 {
		t.Fatalf("planner calls = %d, want 1", n)
	}
	mustContain(t, reply, "ce1:ok:")
	if len(f.workouts.Items) != 1 {
		t.Errorf("workouts = %d, want only the seeded one: %+v", len(f.workouts.Items), f.workouts.Items)
	}
	if f.store.Peek("U1") == nil {
		t.Error("a pending edit card must exist")
	}
}
