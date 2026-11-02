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

func TestMealRepo_FindByIDAndUpdateAreScopedToTheOwner(t *testing.T) {
	repo := meal.NewMealRepository(getTestDB(t))
	ctx := context.Background()
	m := &meal.Meal{
		UserID: "U1", EatenAt: time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC), Source: meal.SourceText, MealType: "lunch", Note: "午餐吃了牛肉麵",
		Items: []meal.FoodItem{{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}}},
	}
	m.Recalc()
	if err := repo.Insert(ctx, m); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if other, err := repo.FindByID(ctx, "U2", m.ID); err != nil || other != nil {
		t.Fatalf("another user must not read it: %+v %v", other, err)
	}
	got, err := repo.FindByID(ctx, "U1", m.ID)
	if err != nil || got == nil || got.Note != "午餐吃了牛肉麵" || got.MealType != "lunch" || got.Total.Calories != 650 {
		t.Fatalf("find: %+v %v", got, err)
	}

	got.SetScale(0.5)
	got.Place = &meal.Place{Name: "永康牛肉麵", Source: meal.PlaceFromUser}
	if ok, err := repo.Update(ctx, got); err != nil || !ok {
		t.Fatalf("update: %v %v", ok, err)
	}
	stored, _ := repo.FindByID(ctx, "U1", m.ID)
	if stored.Total.Calories != 325 || stored.Scale != 0.5 || stored.Place == nil || stored.Place.Name != "永康牛肉麵" ||
		stored.Items[0].Base == nil || stored.Items[0].Base.Calories != 650 {
		t.Fatalf("stored = %+v", stored)
	}
	stored.SetScale(1)
	if stored.Total.Calories != 650 {
		t.Errorf("the stored base restores the estimate exactly: %v", stored.Total.Calories)
	}

	hijack := *stored
	hijack.UserID = "U2"
	hijack.Total.Calories = 1
	if ok, err := repo.Update(ctx, &hijack); err != nil || ok {
		t.Errorf("another user must not overwrite it: %v %v", ok, err)
	}
	if missing, err := repo.FindByID(ctx, "U1", "no-such-id"); err != nil || missing != nil {
		t.Errorf("missing id: %+v %v", missing, err)
	}
}
