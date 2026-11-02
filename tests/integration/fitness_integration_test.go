package integration

import (
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"testing"
	"time"
)

func TestWorkoutRepo_InsertFindDeleteAll(t *testing.T) {
	repo := workout.NewWorkoutRepository(getTestDB(t))
	ctx := context.Background()
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

	for i, hour := range []int{1, 5} {
		w := &workout.Workout{UserID: "U1", Activity: "跑步", DurationMin: 30, Calories: float64(300 + i), DoneAt: base.Add(time.Duration(hour) * time.Hour)}
		if err := repo.Insert(ctx, w); err != nil || w.ID == "" {
			t.Fatalf("insert: %v id=%q", err, w.ID)
		}
	}
	got, err := repo.FindByUserBetween(ctx, "U1", base, base.Add(24*time.Hour))
	if err != nil || len(got) != 2 || got[0].Calories != 300 {
		t.Fatalf("find: %+v %v", got, err)
	}
	deleted, err := repo.DeleteLatest(ctx, "U1", base)
	if err != nil || deleted == nil || deleted.Calories != 301 {
		t.Fatalf("delete latest: %+v %v", deleted, err)
	}
	if err := repo.DeleteAllByUser(ctx, "U1"); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if got, _ := repo.FindByUserBetween(ctx, "U1", base, base.Add(24*time.Hour)); len(got) != 0 {
		t.Errorf("left: %+v", got)
	}
}

func TestProfileRepo_UpsertGetDelete(t *testing.T) {
	repo := profile.NewProfileRepository(getTestDB(t))
	ctx := context.Background()

	if p, err := repo.Get(ctx, "U1"); err != nil || p != nil {
		t.Fatalf("missing profile: %+v %v", p, err)
	}
	if err := repo.Upsert(ctx, &profile.Profile{UserID: "U1", WeightKG: 70, Age: 30, UpdateTime: time.Now()}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.Upsert(ctx, &profile.Profile{UserID: "U1", WeightKG: 68, Age: 30, UpdateTime: time.Now()}); err != nil {
		t.Fatalf("update: %v", err)
	}
	p, err := repo.Get(ctx, "U1")
	if err != nil || p == nil || p.WeightKG != 68 || p.Age != 30 {
		t.Fatalf("get: %+v %v", p, err)
	}
	if err := repo.Delete(ctx, "U1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p, _ := repo.Get(ctx, "U1"); p != nil {
		t.Errorf("not deleted: %+v", p)
	}
}
