package alertsub

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// AlertSubRepository 警報訂閱儲存庫介面.
type AlertSubRepository interface {
	FindByUserID(ctx context.Context, userID string) ([]AlertSubscription, error)
	FindEnabledByType(ctx context.Context, alertType string) ([]AlertSubscription, error)
	FindEnabledByTypeAndRegions(ctx context.Context, alertType string, regions []string) ([]AlertSubscription, error)
	HasEnabledByType(ctx context.Context, alertType string) (bool, error)
	Upsert(ctx context.Context, sub *AlertSubscription) error
	Delete(ctx context.Context, id string) error
	// DeleteByUser 刪除使用者所有訂閱（刪除個人資料時使用）.
	DeleteByUser(ctx context.Context, userID string) error
}

// AlertSubRepo 警報訂閱儲存庫實現.
type AlertSubRepo struct {
	collection *mongo.Collection
}

// NewAlertSubRepository 建立警報訂閱儲存庫.
func NewAlertSubRepository(db *mongo.Database) *AlertSubRepo {
	ctx := context.Background()
	coll := db.Collection("alertSubscription")

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "alertType", Value: 1}}},
		{Keys: bson.D{{Key: "alertType", Value: 1}, {Key: "enabled", Value: 1}}},
		{Keys: bson.D{{Key: "alertType", Value: 1}, {Key: "enabled", Value: 1}, {Key: "regions", Value: 1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		slog.Warn("alertsub index creation", "error", err)
	}

	return &AlertSubRepo{collection: coll}
}

func (r *AlertSubRepo) FindByUserID(ctx context.Context, userID string) ([]AlertSubscription, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"userId": userID})
	if err != nil {
		return nil, fmt.Errorf("find by userID: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []AlertSubscription
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return results, nil
}

func (r *AlertSubRepo) FindEnabledByType(ctx context.Context, alertType string) ([]AlertSubscription, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"alertType": alertType, "enabled": true})
	if err != nil {
		return nil, fmt.Errorf("find enabled by type: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []AlertSubscription
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return results, nil
}

// FindEnabledByTypeAndRegions 訂閱地區含 regions 任一個的啟用中訂閱；每筆訂閱只回一次.
// [F24-99] regions 為空時不查資料庫（沒有要比對的地區）.
func (r *AlertSubRepo) FindEnabledByTypeAndRegions(
	ctx context.Context, alertType string, regions []string,
) ([]AlertSubscription, error) {
	if len(regions) == 0 {
		return nil, nil
	}
	filter := bson.M{
		"alertType": alertType,
		"enabled":   true,
		"regions":   bson.M{"$in": regions},
	}
	cursor, err := r.collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("find enabled by type and regions: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []AlertSubscription
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return results, nil
}

// HasEnabledByType 是否有任何人開啟這類通知.
// [F24-99] 警報檢查先問這個：沒有人開啟就不呼叫氣象署.
func (r *AlertSubRepo) HasEnabledByType(ctx context.Context, alertType string) (bool, error) {
	n, err := r.collection.CountDocuments(ctx, bson.M{"alertType": alertType, "enabled": true}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("count enabled by type: %w", err)
	}
	return n > 0, nil
}

func (r *AlertSubRepo) Upsert(ctx context.Context, sub *AlertSubscription) error {
	sub.UpdateTime = time.Now()
	if sub.CreateTime.IsZero() {
		sub.CreateTime = sub.UpdateTime
	}

	filter := bson.M{"_id": sub.ID}
	update := bson.M{"$set": sub}
	opts := options.UpdateOne().SetUpsert(true)

	_, err := r.collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		// [F24-96] sub.ID 含 userID，log 只記 alertType.
		slog.Error("alertsub upsert failed", "alertType", sub.AlertType, "error", err)
		return fmt.Errorf("upsert alert subscription: %w", err)
	}
	return nil
}

// DeleteByUser 刪除使用者所有訂閱.
func (r *AlertSubRepo) DeleteByUser(ctx context.Context, userID string) error {
	if _, err := r.collection.DeleteMany(ctx, bson.M{"userId": userID}); err != nil {
		return fmt.Errorf("delete user alert subscriptions: %w", err)
	}
	return nil
}

func (r *AlertSubRepo) Delete(ctx context.Context, id string) error {
	_, err := r.collection.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("delete alert subscription: %w", err)
	}
	return nil
}
