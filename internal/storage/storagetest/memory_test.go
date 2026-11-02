package storagetest

import (
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

func TestProfilesMeetTheContract(t *testing.T) {
	ProfileRepositoryContract(t, func(*testing.T) profile.ProfileRepository { return NewProfiles() })
}
