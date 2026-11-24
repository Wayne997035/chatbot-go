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

	"chatbot-go/internal/storage/dblog"
)

// WorkoutRepository 運動紀錄儲存庫介面.
type WorkoutRepository interface {
	Insert(ctx context.Context, w *Workout) error
	FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]Workout, error)
	// DeleteLatest 刪除 since 之後最新的一筆；沒有可刪的回傳 nil, nil.
	DeleteLatest(ctx context.Context, userID string, since time.Time) (*Workout, error)
	DeleteAllByUser(ctx context.Context, userID string) error
	// DeleteByID 刪除使用者自己的一筆紀錄；不存在或不屬於該使用者時回傳 false.
	DeleteByID(ctx context.Context, userID, id string) (bool, error)
	// FindByID [F33-20] 取得使用者自己的一筆紀錄；不存在或不屬於該使用者時回傳 nil, nil.
	FindByID(ctx context.Context, userID, id string) (*Workout, error)
	// Update [F33-20] 整筆寫回使用者自己的紀錄並把 w.Version 加 1；不存在或不屬於該使用者時回傳 false，
	// 讀出之後被別人改過（版本不符）時回傳 ErrConflict.
	Update(ctx context.Context, w *Workout) (bool, error)
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
	_, err := r.collection.InsertOne(ctx, w)
	dblog.Write(ctx, "workout", dblog.ActionInsert, err) // [F37-08]
	if err != nil {
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
	_, err := r.collection.DeleteMany(ctx, bson.M{"userId": userID})
	dblog.Write(ctx, "workout", dblog.ActionDeleteMany, err) // [F37-08]
	if err != nil {
		return fmt.Errorf("delete workouts: %w", err)
	}
	return nil
}

// DeleteByID 刪除使用者自己的一筆紀錄（同時比對 userId，避免刪到別人的資料）.
func (r *WorkoutRepo) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	res, err := r.collection.DeleteOne(ctx, bson.M{"_id": id, "userId": userID})
	dblog.Write(ctx, "workout", dblog.ActionDelete, err) // [F37-08]
	if err != nil {
		return false, fmt.Errorf("delete workout: %w", err)
	}
	return res.DeletedCount == 1, nil
}

// FindByID [F33-20] 取得使用者自己的一筆紀錄（同時比對 userId，別人的 ID 一律當作不存在）.
func (r *WorkoutRepo) FindByID(ctx context.Context, userID, id string) (*Workout, error) {
	var w Workout
	if err := r.collection.FindOne(ctx, bson.M{"_id": id, "userId": userID}).Decode(&w); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("find workout: %w", err)
	}
	return &w, nil
}

// Update [F33-20] 整筆更新使用者自己的紀錄；同時比對 _id、userId 與 version（CAS），成功時 w.Version 加 1.
// 舊文件沒有 version 欄位，視為 0（與 meal 的手法相同）.
func (r *WorkoutRepo) Update(ctx context.Context, w *Workout) (bool, error) {
	owned := bson.M{"_id": w.ID, "userId": w.UserID}
	filter := bson.M{"_id": w.ID, "userId": w.UserID, "version": w.Version}
	if w.Version == 0 {
		filter["version"] = bson.M{"$in": bson.A{0, nil}}
	}
	next := *w
	next.Version++
	res, err := r.collection.ReplaceOne(ctx, filter, &next)
	dblog.Write(ctx, "workout", dblog.ActionReplace, err) // [F37-08]
	if err != nil {
		return false, fmt.Errorf("update workout: %w", err)
	}
	if res.MatchedCount == 1 {
		w.Version = next.Version
		return true, nil
	}
	n, err := r.collection.CountDocuments(ctx, owned)
	if err != nil {
		return false, fmt.Errorf("check workout: %w", err)
	}
	if n > 0 {
		return false, ErrConflict
	}
	return false, nil
}
