package alertsub

import "time"

// AlertType 常數.
const (
	TypeEarthquakeTsunami = "earthquake_tsunami"
	TypeWeatherWarning    = "weather_warning"
)

// AlertSubscription 使用者警報訂閱資料.
type AlertSubscription struct {
	ID         string    `bson:"_id"        json:"id"` // "{userID}_{alertType}"
	UserID     string    `bson:"userId"     json:"userId"`
	AlertType  string    `bson:"alertType"  json:"alertType"` // earthquake_tsunami | weather_warning
	Regions    []string  `bson:"regions"    json:"regions"`   // 天氣特報訂閱地區；地震/海嘯為空
	Enabled    bool      `bson:"enabled"    json:"enabled"`
	CreateTime time.Time `bson:"createTime" json:"createTime"`
	UpdateTime time.Time `bson:"updateTime" json:"updateTime"`
}
