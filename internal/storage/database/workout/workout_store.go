package workout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WorkoutRepository 運動紀錄儲存庫介面.
type WorkoutRepository interface {
	Insert(ctx context.Context, w *Workout) error
	FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]Workout, error)
	// DeleteLatest 刪除 since 之後最新的一筆；沒有可刪的回傳 nil, nil.
	DeleteLatest(ctx context.Context, userID string, since time.Time) (*Workout, error)
	DeleteAllByUser(ctx context.Context, userID string) error
}

// WorkoutRepo 運動紀錄儲存庫實現.
type WorkoutRepo struct {
	collection *mongo.Collection
}

// NewWorkoutRepository 建立運動紀錄儲存庫.
func NewWorkoutRepository(db *mongo.Database) *WorkoutRepo {
	coll := db.Collection("workout")
	index := mongo.IndexModel{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "doneAt", Value: -1}}}
	if _, err := coll.Indexes().CreateOne(context.Background(), index); err != nil {
		slog.Warn("workout index creation", "error", err)
	}
	return &WorkoutRepo{collection: coll}
}

// Insert 新增一筆；ID 為空時自動產生.
func (r *WorkoutRepo) Insert(ctx context.Context, w *Workout) error {
	if w.ID == "" {
		w.ID = bson.NewObjectID().Hex()
	}
	if _, err := r.collection.InsertOne(ctx, w); err != nil {
		return fmt.Errorf("insert workout: %w", err)
	}
	return nil
}

// FindByUserBetween 查詢 [from, to) 期間的紀錄，依時間排序.
func (r *WorkoutRepo) FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]Workout, error) {
	filter := bson.M{"userId": userID, "doneAt": bson.M{"$gte": from, "$lt": to}}
	cursor, err := r.collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "doneAt", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("find workouts: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var workouts []Workout
	if err := cursor.All(ctx, &workouts); err != nil {
		return nil, fmt.Errorf("decode workouts: %w", err)
	}
	return workouts, nil
}

// DeleteLatest 刪除 since 之後最新的一筆.
func (r *WorkoutRepo) DeleteLatest(ctx context.Context, userID string, since time.Time) (*Workout, error) {
	filter := bson.M{"userId": userID, "doneAt": bson.M{"$gte": since}}
	opts := options.FindOneAndDelete().SetSort(bson.D{{Key: "doneAt", Value: -1}})
	var w Workout
	if err := r.collection.FindOneAndDelete(ctx, filter, opts).Decode(&w); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("delete latest workout: %w", err)
	}
	return &w, nil
}

// DeleteAllByUser 刪除使用者所有運動紀錄.
func (r *WorkoutRepo) DeleteAllByUser(ctx context.Context, userID string) error {
	if _, err := r.collection.DeleteMany(ctx, bson.M{"userId": userID}); err != nil {
		return fmt.Errorf("delete workouts: %w", err)
	}
	return nil
}
