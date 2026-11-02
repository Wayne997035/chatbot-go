package integration

import (
	"chatbot-go/internal/storage/database/meal"
	"context"
	"testing"
	"time"
)

func TestMealRepo_InsertFindDelete(t *testing.T) {
	repo := meal.NewMealRepository(getTestDB(t))
	ctx := context.Background()
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

	for i, hour := range []int{1, 5, 9} {
		m := &meal.Meal{
			UserID:  "U1",
			EatenAt: base.Add(time.Duration(hour) * time.Hour),
			Items:   []meal.FoodItem{{Name: "飯", Nutrients: meal.Nutrients{Calories: float64(100 * (i + 1))}}},
			Total:   meal.Nutrients{Calories: float64(100 * (i + 1))},
		}
		if err := repo.Insert(ctx, m); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if m.ID == "" {
			t.Fatal("insert should assign an ID")
		}
	}
	if err := repo.Insert(ctx, &meal.Meal{UserID: "U2", EatenAt: base.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("insert other user: %v", err)
	}

	meals, err := repo.FindByUserBetween(ctx, "U1", base, base.Add(6*time.Hour))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(meals) != 2 || meals[0].Total.Calories != 100 || meals[1].Items[0].Name != "飯" {
		t.Fatalf("meals = %+v", meals)
	}

	deleted, err := repo.DeleteLatest(ctx, "U1", base)
	if err != nil || deleted == nil || deleted.Total.Calories != 300 {
		t.Fatalf("deleted=%+v err=%v", deleted, err)
	}
	if none, err := repo.DeleteLatest(ctx, "U1", base.Add(10*time.Hour)); err != nil || none != nil {
		t.Errorf("nothing to delete: %+v %v", none, err)
	}
}
