package database

import (
	"chatbot-go/internal/platform/driver"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/storage/database/workout"
)

// Repositories 儲存庫集合.
type Repositories struct {
	User     userstore.UserRepository
	Weather  weatherstore.WeatherRepository
	AlertSub alertsub.AlertSubRepository
	Meal     meal.MealRepository
	Workout  workout.WorkoutRepository
	Profile  profile.ProfileRepository
}

// NewRepositories 建立儲存庫集合.
func NewRepositories() *Repositories {
	db := driver.GetMongoDatabase()
	if db == nil {
		panic("資料庫連接失敗，無法建立 Repositories")
	}

	return &Repositories{
		User:     userstore.NewUserRepository(db),
		Weather:  weatherstore.NewWeatherRepository(db),
		AlertSub: alertsub.NewAlertSubRepository(db),
		Meal:     meal.NewMealRepository(db),
		Workout:  workout.NewWorkoutRepository(db),
		Profile:  profile.NewProfileRepository(db),
	}
}
