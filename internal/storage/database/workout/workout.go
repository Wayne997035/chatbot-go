// Package workout 運動紀錄儲存.
package workout

import (
	"errors"
	"time"
)

// ErrConflict [F33-20] 讀出之後被別人改過（版本不符）；與「不存在或不屬於該使用者」（false, nil）不同.
var ErrConflict = errors.New("workout: version conflict")

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
	// Version [F33-20] 樂觀鎖版本；舊文件沒有這個欄位視為 0. Update 成功時加 1.
	Version int64 `bson:"version"`
}
