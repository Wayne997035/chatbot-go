package storagetest

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"errors"
	"testing"
	"time"
)

// 契約測試：儲存庫的行為規格. 記憶體實作（這個套件的測試）與 MongoDB 實作（tests/integration）都要通過，
// 確保其他套件用記憶體實作測出來的結果，在真的資料庫上也成立.
//
// 時間都用整秒（MongoDB 只存到毫秒），每個子測試都拿一個全新的儲存庫.

// day 契約測試的基準時間.
var day = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func at(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func newMeal(userID string, eatenAt, createdAt time.Time, calories float64) *meal.Meal {
	m := &meal.Meal{
		UserID: userID, EatenAt: eatenAt, CreateTime: createdAt, MealType: "lunch", Note: "午餐吃了牛肉麵",
		Items: []meal.FoodItem{{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: calories, ProteinG: 30}}},
	}
	m.Recalc()
	return m
}

// MealRepositoryContract meal.MealRepository 的行為規格.
func MealRepositoryContract(t *testing.T, newRepo func(t *testing.T) meal.MealRepository) {
	ctx := context.Background()
	insert := func(t *testing.T, repo meal.MealRepository, m *meal.Meal) *meal.Meal {
		t.Helper()
		if err := repo.Insert(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("insert assigns an ID and a create time and keeps every field", func(t *testing.T) {
		repo := newRepo(t)
		m := newMeal("U1", at(12, 0), time.Time{}, 650)
		m.Place = &meal.Place{Name: "老張牛肉麵", Source: meal.PlaceFromUser}
		insert(t, repo, m)
		if m.ID == "" || m.CreateTime.IsZero() {
			t.Fatalf("id=%q createTime=%v", m.ID, m.CreateTime)
		}
		got, err := repo.FindByID(ctx, "U1", m.ID)
		if err != nil || got == nil || !got.EatenAt.Equal(m.EatenAt) || got.Note != m.Note || got.Total.Calories != 650 ||
			len(got.Items) != 1 || got.Items[0].Name != "牛肉麵" || got.Place == nil || got.Place.Name != "老張牛肉麵" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})

	t.Run("find between is per user, half-open and ordered by eaten time", func(t *testing.T) {
		repo := newRepo(t)
		for _, h := range []int{3, 1, 2, 6} {
			insert(t, repo, newMeal("U1", at(h, 0), at(10, h), float64(100*h)))
		}
		insert(t, repo, newMeal("U2", at(1, 30), at(10, 0), 999))
		got, err := repo.FindByUserBetween(ctx, "U1", at(1, 0), at(6, 0))
		if err != nil || len(got) != 3 || got[0].Total.Calories != 100 || got[1].Total.Calories != 200 || got[2].Total.Calories != 300 {
			t.Fatalf("got %+v err %v", got, err)
		}
	})

	t.Run("delete latest picks the last one logged since the given time", func(t *testing.T) {
		repo := newRepo(t)
		insert(t, repo, newMeal("U1", at(7, 0), at(8, 0), 300))   // 記錄時間早於 since
		insert(t, repo, newMeal("U1", at(12, 0), at(15, 0), 650)) // 午餐
		insert(t, repo, newMeal("U1", at(8, 0), at(15, 1), 400))  // 下午才補記的早餐
		insert(t, repo, newMeal("U2", at(9, 0), at(16, 0), 999))
		for _, want := range []float64{400, 650} {
			got, err := repo.DeleteLatest(ctx, "U1", at(14, 0))
			if err != nil || got == nil || got.Total.Calories != want {
				t.Fatalf("deleted %+v err %v, want %v kcal", got, err, want)
			}
		}
		if got, err := repo.DeleteLatest(ctx, "U1", at(14, 0)); got != nil || err != nil {
			t.Fatalf("nothing left since 14:00: %+v %v", got, err)
		}
		if left, _ := repo.FindByUserBetween(ctx, "U2", day, at(24, 0)); len(left) != 1 {
			t.Errorf("another user's meal must stay: %+v", left)
		}
	})

	t.Run("delete all only removes that user's meals", func(t *testing.T) {
		repo := newRepo(t)
		insert(t, repo, newMeal("U1", at(12, 0), at(12, 0), 650))
		insert(t, repo, newMeal("U2", at(12, 0), at(12, 0), 500))
		if err := repo.DeleteAllByUser(ctx, "U1"); err != nil {
			t.Fatal(err)
		}
		mine, _ := repo.FindByUserBetween(ctx, "U1", day, at(24, 0))
		theirs, _ := repo.FindByUserBetween(ctx, "U2", day, at(24, 0))
		if len(mine) != 0 || len(theirs) != 1 {
			t.Errorf("mine=%d theirs=%d", len(mine), len(theirs))
		}
	})

	t.Run("find, update and delete by ID are scoped to the owner", func(t *testing.T) {
		repo := newRepo(t)
		m := insert(t, repo, newMeal("U1", at(12, 0), at(12, 0), 650))
		if other, err := repo.FindByID(ctx, "U2", m.ID); other != nil || err != nil {
			t.Fatalf("another user read it: %+v %v", other, err)
		}
		hijack := *m
		hijack.UserID, hijack.Note = "U2", "改掉"
		if ok, err := repo.Update(ctx, &hijack); ok || err != nil {
			t.Fatalf("another user's update: ok=%v err=%v (must be not-found, not a conflict)", ok, err)
		}
		if ok, err := repo.DeleteByID(ctx, "U2", m.ID); ok || err != nil {
			t.Fatalf("another user's delete: ok=%v err=%v", ok, err)
		}
		if ok, err := repo.DeleteByID(ctx, "U1", m.ID); !ok || err != nil {
			t.Fatalf("owner's delete: ok=%v err=%v", ok, err)
		}
		if got, _ := repo.FindByID(ctx, "U1", m.ID); got != nil {
			t.Errorf("still there: %+v", got)
		}
		if ok, err := repo.Update(ctx, m); ok || err != nil {
			t.Errorf("update of a deleted meal: ok=%v err=%v", ok, err)
		}
	})

	t.Run("update rejects a stale version and bumps the version", func(t *testing.T) {
		repo := newRepo(t)
		m := insert(t, repo, newMeal("U1", at(12, 0), at(12, 0), 650))
		a, _ := repo.FindByID(ctx, "U1", m.ID)
		b, _ := repo.FindByID(ctx, "U1", m.ID)
		a.Note = "first"
		if ok, err := repo.Update(ctx, a); !ok || err != nil || a.Version != m.Version+1 {
			t.Fatalf("first write: ok=%v err=%v version=%d", ok, err, a.Version)
		}
		b.Note = "second"
		if ok, err := repo.Update(ctx, b); ok || !errors.Is(err, meal.ErrConflict) {
			t.Fatalf("stale write: ok=%v err=%v", ok, err)
		}
		if got, _ := repo.FindByID(ctx, "U1", m.ID); got.Note != "first" || got.Version != a.Version {
			t.Errorf("stored %+v", got)
		}
	})

	t.Run("found meals are copies", func(t *testing.T) {
		repo := newRepo(t)
		m := insert(t, repo, newMeal("U1", at(12, 0), at(12, 0), 650))
		got, _ := repo.FindByID(ctx, "U1", m.ID)
		got.Items[0].Name = "改掉"
		got.Note = "改掉"
		if again, _ := repo.FindByID(ctx, "U1", m.ID); again.Items[0].Name != "牛肉麵" || again.Note != m.Note {
			t.Errorf("changing a found meal changed the stored one: %+v", again)
		}
	})
}

// WorkoutRepositoryContract workout.WorkoutRepository 的行為規格.
func WorkoutRepositoryContract(t *testing.T, newRepo func(t *testing.T) workout.WorkoutRepository) {
	ctx := context.Background()
	newWorkout := func(userID string, doneAt time.Time, calories float64) *workout.Workout {
		return &workout.Workout{UserID: userID, Activity: "跑步", DurationMin: 30, Calories: calories, Source: workout.SourceText, DoneAt: doneAt}
	}
	insert := func(t *testing.T, repo workout.WorkoutRepository, w *workout.Workout) *workout.Workout {
		t.Helper()
		if err := repo.Insert(ctx, w); err != nil || w.ID == "" {
			t.Fatalf("insert: id=%q err=%v", w.ID, err)
		}
		return w
	}

	t.Run("find between is per user, half-open and ordered by time", func(t *testing.T) {
		repo := newRepo(t)
		for _, h := range []int{3, 1, 2, 6} {
			insert(t, repo, newWorkout("U1", at(h, 0), float64(100*h)))
		}
		insert(t, repo, newWorkout("U2", at(2, 0), 999))
		got, err := repo.FindByUserBetween(ctx, "U1", at(1, 0), at(6, 0))
		if err != nil || len(got) != 3 || got[0].Calories != 100 || got[2].Calories != 300 {
			t.Fatalf("got %+v err %v", got, err)
		}
	})

	t.Run("delete latest, delete by ID and delete all are scoped", func(t *testing.T) {
		repo := newRepo(t)
		insert(t, repo, newWorkout("U1", at(9, 0), 100))
		insert(t, repo, newWorkout("U1", at(18, 0), 300))
		w := insert(t, repo, newWorkout("U1", at(12, 0), 200))
		insert(t, repo, newWorkout("U2", at(20, 0), 999))
		if got, err := repo.DeleteLatest(ctx, "U1", at(10, 0)); err != nil || got == nil || got.Calories != 300 {
			t.Fatalf("delete latest: %+v %v", got, err)
		}
		if ok, _ := repo.DeleteByID(ctx, "U2", w.ID); ok {
			t.Fatal("another user deleted it")
		}
		if ok, err := repo.DeleteByID(ctx, "U1", w.ID); !ok || err != nil {
			t.Fatalf("delete by ID: %v %v", ok, err)
		}
		if got, err := repo.DeleteLatest(ctx, "U1", at(10, 0)); got != nil || err != nil {
			t.Fatalf("nothing left since 10:00: %+v %v", got, err)
		}
		if err := repo.DeleteAllByUser(ctx, "U1"); err != nil {
			t.Fatal(err)
		}
		mine, _ := repo.FindByUserBetween(ctx, "U1", day, at(24, 0))
		theirs, _ := repo.FindByUserBetween(ctx, "U2", day, at(24, 0))
		if len(mine) != 0 || len(theirs) != 1 {
			t.Errorf("mine=%d theirs=%d", len(mine), len(theirs))
		}
	})
}

// ProfileRepositoryContract profile.ProfileRepository 的行為規格.
func ProfileRepositoryContract(t *testing.T, newRepo func(t *testing.T) profile.ProfileRepository) {
	ctx := context.Background()

	t.Run("missing profile is nil", func(t *testing.T) {
		if p, err := newRepo(t).Get(ctx, "U1"); p != nil || err != nil {
			t.Errorf("got %+v %v", p, err)
		}
	})

	t.Run("upsert replaces the whole profile", func(t *testing.T) {
		repo := newRepo(t)
		_ = repo.Upsert(ctx, &profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale, UpdateTime: day})
		_ = repo.Upsert(ctx, &profile.Profile{UserID: "U1", WeightKG: 72, UpdateTime: day})
		if p, _ := repo.Get(ctx, "U1"); p == nil || p.WeightKG != 72 || p.HeightCM != 0 || p.Age != 0 || p.Sex != "" {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("merge only sets the given fields and creates when missing", func(t *testing.T) {
		repo := newRepo(t)
		p, err := repo.Merge(ctx, &profile.Profile{UserID: "U1", WeightKG: 70, UpdateTime: day})
		if err != nil || p.WeightKG != 70 || p.HeightCM != 0 || p.UserID != "U1" {
			t.Fatalf("create: %+v %v", p, err)
		}
		p, err = repo.Merge(ctx, &profile.Profile{UserID: "U1", HeightCM: 175, Sex: profile.SexFemale, UpdateTime: day})
		if err != nil || p.WeightKG != 70 || p.HeightCM != 175 || p.Sex != profile.SexFemale || p.Age != 0 {
			t.Fatalf("merge: %+v %v", p, err)
		}
		p, err = repo.Merge(ctx, &profile.Profile{UserID: "U1", Age: 31, UpdateTime: day})
		if err != nil || p.WeightKG != 70 || p.HeightCM != 175 || p.Sex != profile.SexFemale || p.Age != 31 {
			t.Fatalf("fields not in the patch must be kept: %+v %v", p, err)
		}
		if stored, _ := repo.Get(ctx, "U1"); stored == nil || stored.WeightKG != 70 || stored.HeightCM != 175 || stored.Age != 31 {
			t.Errorf("stored %+v", stored)
		}
	})

	t.Run("delete removes only that user", func(t *testing.T) {
		repo := newRepo(t)
		_ = repo.Upsert(ctx, &profile.Profile{UserID: "U1", WeightKG: 70, UpdateTime: day})
		_ = repo.Upsert(ctx, &profile.Profile{UserID: "U2", WeightKG: 60, UpdateTime: day})
		if err := repo.Delete(ctx, "U1"); err != nil {
			t.Fatal(err)
		}
		mine, _ := repo.Get(ctx, "U1")
		theirs, _ := repo.Get(ctx, "U2")
		if mine != nil || theirs == nil {
			t.Errorf("mine=%+v theirs=%+v", mine, theirs)
		}
	})
}
