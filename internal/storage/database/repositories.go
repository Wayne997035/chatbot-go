package database

import (
	"chatbot-go/internal/platform/driver"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
)

// Repositories 儲存庫集合.
type Repositories struct {
	User    userstore.UserRepository
	Weather weatherstore.WeatherRepository
}

// NewRepositories 建立儲存庫集合.
func NewRepositories() *Repositories {
	db := driver.GetMongoDatabase()
	if db == nil {
		panic("資料庫連接失敗，無法建立 Repositories")
	}

	return &Repositories{
		User:    userstore.NewUserRepository(db),
		Weather: weatherstore.NewWeatherRepository(db),
	}
}
