// Package workout 運動紀錄儲存.
package workout

import "time"

// 紀錄來源.
const (
	SourceText       = "text"
	SourceAI         = "ai"
	SourceScreenshot = "screenshot"
)

// Workout 一筆運動紀錄.
type Workout struct {
	ID          string    `bson:"_id"                  json:"id"`
	UserID      string    `bson:"userId"               json:"-"`
	Activity    string    `bson:"activity"             json:"activity"`
	DurationMin float64   `bson:"durationMin"          json:"durationMin"`
	DistanceKM  float64   `bson:"distanceKm,omitempty" json:"distanceKm,omitempty"`
	Calories    float64   `bson:"calories"             json:"calories"`
	Source      string    `bson:"source"               json:"source"`
	DoneAt      time.Time `bson:"doneAt"               json:"doneAt"`
	CreateTime  time.Time `bson:"createTime"           json:"-"`
}
