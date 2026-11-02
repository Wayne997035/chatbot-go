package integration

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 兩邊各自讀出同一筆、先寫的成功，後寫的因為版本不符被拒絕（不會蓋掉先寫的）.
func TestMealRepo_UpdateRejectsAStaleVersion(t *testing.T) {
	repo := meal.NewMealRepository(getTestDB(t))
	ctx := context.Background()
	m := &meal.Meal{UserID: "U1", EatenAt: time.Now(), Items: []meal.FoodItem{{Name: "飯", Nutrients: meal.Nutrients{Calories: 200}}}}
	m.Recalc()
	if err := repo.Insert(ctx, m); err != nil {
		t.Fatal(err)
	}
	a, _ := repo.FindByID(ctx, "U1", m.ID)
	b, _ := repo.FindByID(ctx, "U1", m.ID)

	a.Note = "first"
	if ok, err := repo.Update(ctx, a); !ok || err != nil || a.Version != 1 {
		t.Fatalf("first write: ok=%v err=%v version=%d", ok, err, a.Version)
	}
	b.Note = "second"
	if ok, err := repo.Update(ctx, b); ok || !errors.Is(err, meal.ErrConflict) {
		t.Fatalf("a stale write must be rejected: ok=%v err=%v", ok, err)
	}
	got, _ := repo.FindByID(ctx, "U1", m.ID)
	if got.Note != "first" || got.Version != 1 {
		t.Errorf("stored = %+v", got)
	}

	other := *got
	other.UserID = "U2"
	if ok, err := repo.Update(ctx, &other); ok || err != nil {
		t.Errorf("another user's write is not found (not a conflict): ok=%v err=%v", ok, err)
	}
}

// 加版本號之前存的紀錄沒有 version 欄位，也要能修改.
func TestMealRepo_LegacyMealWithoutVersionCanBeModified(t *testing.T) {
	db := getTestDB(t)
	repo := meal.NewMealRepository(db)
	ctx := context.Background()
	now := time.Now()
	legacy := bson.M{
		"_id": "legacy1", "userId": "U1", "eatenAt": now, "createTime": now,
		"items": bson.A{bson.M{"name": "便當", "portion": "1個", "calories": 800}}, "total": bson.M{"calories": 800},
	}
	if _, err := db.Collection("meal").InsertOne(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	got, err := meal.Modify(ctx, repo, "U1", "legacy1", func(m *meal.Meal) { m.SetScale(0.5) })
	if err != nil || got == nil || got.Version != 1 {
		t.Fatalf("modify legacy meal: %+v %v", got, err)
	}
	stored, _ := repo.FindByID(ctx, "U1", "legacy1")
	if stored.Total.Calories != 400 || stored.Version != 1 {
		t.Errorf("stored = %+v", stored)
	}
}

// 多個請求同時修改同一筆：成功的每一個修改都要留下（沒有遺失的更新）；搶輸太多次的回傳 ErrConflict.
func TestMealModify_NoLostUpdatesUnderConcurrency(t *testing.T) {
	repo := meal.NewMealRepository(getTestDB(t))
	ctx := context.Background()
	m := &meal.Meal{UserID: "U1", EatenAt: time.Now()}
	if err := repo.Insert(ctx, m); err != nil {
		t.Fatal(err)
	}

	const writers = 6
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := range writers {
		wg.Go(func() {
			_, err := meal.Modify(ctx, repo, "U1", m.ID, func(m *meal.Meal) {
				m.Items = append(m.Items, meal.FoodItem{Name: fmt.Sprintf("item%d", i)})
			})
			switch {
			case err == nil:
				mu.Lock()
				succeeded++
				mu.Unlock()
			case !errors.Is(err, meal.ErrConflict):
				t.Errorf("modify: %v", err)
			}
		})
	}
	wg.Wait()

	got, _ := repo.FindByID(ctx, "U1", m.ID)
	if succeeded == 0 || len(got.Items) != succeeded || got.Version != int64(succeeded) {
		t.Errorf("succeeded=%d but stored %d items at version %d", succeeded, len(got.Items), got.Version)
	}
}

// Merge 只更新有給的欄位；同時更新不同欄位時全部留下.
func TestProfileRepo_MergeOnlySetsGivenFields(t *testing.T) {
	repo := profile.NewProfileRepository(getTestDB(t))
	ctx := context.Background()
	now := time.Now()

	p, err := repo.Merge(ctx, &profile.Profile{UserID: "U1", WeightKG: 70, UpdateTime: now})
	if err != nil || p.WeightKG != 70 || p.HeightCM != 0 {
		t.Fatalf("first merge creates the profile: %+v %v", p, err)
	}
	p, err = repo.Merge(ctx, &profile.Profile{UserID: "U1", HeightCM: 175, UpdateTime: now})
	if err != nil || p.WeightKG != 70 || p.HeightCM != 175 {
		t.Fatalf("merge keeps the other fields: %+v %v", p, err)
	}

	patches := []profile.Profile{
		{UserID: "U1", WeightKG: 72}, {UserID: "U1", HeightCM: 176}, {UserID: "U1", Age: 31}, {UserID: "U1", Sex: profile.SexFemale},
	}
	var wg sync.WaitGroup
	for i := range patches {
		wg.Go(func() {
			patches[i].UpdateTime = now
			if _, err := repo.Merge(ctx, &patches[i]); err != nil {
				t.Errorf("merge: %v", err)
			}
		})
	}
	wg.Wait()

	got, _ := repo.Get(ctx, "U1")
	if got.WeightKG != 72 || got.HeightCM != 176 || got.Age != 31 || got.Sex != profile.SexFemale {
		t.Errorf("every concurrent field must be kept: %+v", got)
	}
	if other, _ := repo.Get(ctx, "U2"); other != nil {
		t.Errorf("no other profile is created: %+v", other)
	}
}
