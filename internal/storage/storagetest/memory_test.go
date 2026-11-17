package storagetest

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"testing"
)

// 記憶體實作要通過和 MongoDB 實作相同的契約.
func TestMealsMeetTheContract(t *testing.T) {
	MealRepositoryContract(t, func(*testing.T) meal.MealRepository { return &Meals{} })
}

func TestWorkoutsMeetTheContract(t *testing.T) {
	WorkoutRepositoryContract(t, func(*testing.T) workout.WorkoutRepository { return &Workouts{} })
}

// [F33-20] 契約驗收用的具名測試：memory 實作對 workout 契約（含 owner 子測試）.
func TestMemoryWorkoutContract(t *testing.T) {
	WorkoutRepositoryContract(t, func(*testing.T) workout.WorkoutRepository { return &Workouts{} })
}

func TestProfilesMeetTheContract(t *testing.T) {
	ProfileRepositoryContract(t, func(*testing.T) profile.ProfileRepository { return NewProfiles() })
}

// [F33-21] 記憶體版暫存要通過和 Redis、MongoDB 相同的契約.
func TestMemoryPendingStore(t *testing.T) {
	PendingStoreContract(t, func(*testing.T) recordedit.Store { return NewPendingEdits() })
}
