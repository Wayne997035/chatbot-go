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
	ID          string    `bson:"_id"`
	UserID      string    `bson:"userId"`
	Activity    string    `bson:"activity"`
	DurationMin float64   `bson:"durationMin"`
	DistanceKM  float64   `bson:"distanceKm,omitempty"`
	Calories    float64   `bson:"calories"`
	Source      string    `bson:"source"`
	DoneAt      time.Time `bson:"doneAt"`
	CreateTime  time.Time `bson:"createTime"`
}
