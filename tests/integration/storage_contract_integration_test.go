package integration

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"testing"
)

// MongoDB 實作要通過和測試用記憶體實作相同的契約（storagetest），兩邊的行為才一致.
func TestMealRepoMeetsTheContract(t *testing.T) {
	storagetest.MealRepositoryContract(t, func(t *testing.T) meal.MealRepository { return meal.NewMealRepository(getTestDB(t)) })
}

func TestWorkoutRepoMeetsTheContract(t *testing.T) {
	storagetest.WorkoutRepositoryContract(t, func(t *testing.T) workout.WorkoutRepository {
		return workout.NewWorkoutRepository(getTestDB(t))
	})
}

func TestProfileRepoMeetsTheContract(t *testing.T) {
	storagetest.ProfileRepositoryContract(t, func(t *testing.T) profile.ProfileRepository {
		return profile.NewProfileRepository(getTestDB(t))
	})
}
