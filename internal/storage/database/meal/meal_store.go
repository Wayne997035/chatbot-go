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
	DeleteAllByUser(ctx context.Context, userID string) error
	// DeleteByID 刪除使用者自己的一筆紀錄；不存在或不屬於該使用者時回傳 false.
	DeleteByID(ctx context.Context, userID, id string) (bool, error)
	// FindByID 取得使用者自己的一筆紀錄；不存在或不屬於該使用者時回傳 nil, nil.
	FindByID(ctx context.Context, userID, id string) (*Meal, error)
	// Update 整筆更新使用者自己的紀錄；不存在或不屬於該使用者時回傳 false.
	Update(ctx context.Context, m *Meal) (bool, error)
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

// DeleteAllByUser 刪除使用者所有飲食紀錄.
func (r *MealRepo) DeleteAllByUser(ctx context.Context, userID string) error {
	if _, err := r.collection.DeleteMany(ctx, bson.M{"userId": userID}); err != nil {
		return fmt.Errorf("delete meals: %w", err)
	}
	return nil
}

// DeleteByID 刪除使用者自己的一筆紀錄（同時比對 userId，避免刪到別人的資料）.
func (r *MealRepo) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	res, err := r.collection.DeleteOne(ctx, bson.M{"_id": id, "userId": userID})
	if err != nil {
		return false, fmt.Errorf("delete meal: %w", err)
	}
	return res.DeletedCount == 1, nil
}

// FindByID 取得使用者自己的一筆紀錄（同時比對 userId）.
func (r *MealRepo) FindByID(ctx context.Context, userID, id string) (*Meal, error) {
	var m Meal
	if err := r.collection.FindOne(ctx, bson.M{"_id": id, "userId": userID}).Decode(&m); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("find meal: %w", err)
	}
	return &m, nil
}

// Update 整筆更新使用者自己的紀錄（同時比對 userId，避免改到別人的資料）.
func (r *MealRepo) Update(ctx context.Context, m *Meal) (bool, error) {
	res, err := r.collection.ReplaceOne(ctx, bson.M{"_id": m.ID, "userId": m.UserID}, m)
	if err != nil {
		return false, fmt.Errorf("update meal: %w", err)
	}
	return res.MatchedCount == 1, nil
}
