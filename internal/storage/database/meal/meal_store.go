package meal

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

// MealRepository 飲食紀錄儲存庫介面.
type MealRepository interface {
	Insert(ctx context.Context, m *Meal) error
	FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]Meal, error)
	// DeleteLatest 刪除 since 之後最新的一筆；沒有可刪的回傳 nil, nil.
	DeleteLatest(ctx context.Context, userID string, since time.Time) (*Meal, error)
}

// MealRepo 飲食紀錄儲存庫實現.
type MealRepo struct {
	collection *mongo.Collection
}

// NewMealRepository 建立飲食紀錄儲存庫.
func NewMealRepository(db *mongo.Database) *MealRepo {
	coll := db.Collection("meal")
	index := mongo.IndexModel{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "eatenAt", Value: -1}}}
	if _, err := coll.Indexes().CreateOne(context.Background(), index); err != nil {
		slog.Warn("meal index creation", "error", err)
	}
	return &MealRepo{collection: coll}
}

// Insert 新增一餐；ID 為空時自動產生.
func (r *MealRepo) Insert(ctx context.Context, m *Meal) error {
	if m.ID == "" {
		m.ID = bson.NewObjectID().Hex()
	}
	if _, err := r.collection.InsertOne(ctx, m); err != nil {
		return fmt.Errorf("insert meal: %w", err)
	}
	return nil
}

// FindByUserBetween 查詢 [from, to) 期間的紀錄，依時間排序.
func (r *MealRepo) FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]Meal, error) {
	filter := bson.M{"userId": userID, "eatenAt": bson.M{"$gte": from, "$lt": to}}
	cursor, err := r.collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "eatenAt", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("find meals: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var meals []Meal
	if err := cursor.All(ctx, &meals); err != nil {
		return nil, fmt.Errorf("decode meals: %w", err)
	}
	return meals, nil
}

// DeleteLatest 刪除 since 之後最新的一筆.
func (r *MealRepo) DeleteLatest(ctx context.Context, userID string, since time.Time) (*Meal, error) {
	filter := bson.M{"userId": userID, "eatenAt": bson.M{"$gte": since}}
	opts := options.FindOneAndDelete().SetSort(bson.D{{Key: "eatenAt", Value: -1}})
	var m Meal
	if err := r.collection.FindOneAndDelete(ctx, filter, opts).Decode(&m); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("delete latest meal: %w", err)
	}
	return &m, nil
}
