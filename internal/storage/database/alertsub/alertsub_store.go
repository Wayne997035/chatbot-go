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
	FindEnabledByTypeAndRegion(ctx context.Context, alertType, region string) ([]AlertSubscription, error)
	Upsert(ctx context.Context, sub *AlertSubscription) error
	Delete(ctx context.Context, id string) error
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

func (r *AlertSubRepo) FindEnabledByTypeAndRegion(
	ctx context.Context, alertType, region string,
) ([]AlertSubscription, error) {
	filter := bson.M{
		"alertType": alertType,
		"enabled":   true,
		"regions":   region,
	}
	cursor, err := r.collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("find enabled by type and region: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []AlertSubscription
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return results, nil
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
		slog.Error("alertsub upsert failed", "id", sub.ID, "error", err)
		return fmt.Errorf("upsert alert subscription: %w", err)
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
