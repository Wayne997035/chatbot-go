package database

import (
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/pendingedit"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/storage/database/workout"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Repositories 儲存庫集合.
type Repositories struct {
	User     userstore.UserRepository
	Weather  weatherstore.WeatherRepository
	AlertSub alertsub.AlertSubRepository
	Meal     meal.MealRepository
	Workout  workout.WorkoutRepository
	Profile  profile.ProfileRepository
	// PendingEdit [F33-25] 聊天修改紀錄的待確認暫存（沒有 Redis 時使用的 MongoDB 版）.
	PendingEdit *pendingedit.MongoStore
}

// NewRepositories 用已連線的資料庫建立儲存庫集合.
func NewRepositories(db *mongo.Database) *Repositories {
	return &Repositories{
		User:     userstore.NewUserRepository(db),
		Weather:  weatherstore.NewWeatherRepository(db),
		AlertSub: alertsub.NewAlertSubRepository(db),
		Meal:     meal.NewMealRepository(db),
		Workout:  workout.NewWorkoutRepository(db),
		Profile:  profile.NewProfileRepository(db),
		// [F33-25]
		PendingEdit: pendingedit.NewMongoStore(db),
	}
}
