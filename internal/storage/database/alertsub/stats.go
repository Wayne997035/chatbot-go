package alertsub

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// RegionCount [F30-78] 天氣特報訂閱的地區分組結果：Region 是 regions 陣列裡的原字串.
type RegionCount struct {
	Region string
	Count  int64
}

// CountEnabledByType [F30-78] enabled=true 的該類訂閱數.
func (r *AlertSubRepo) CountEnabledByType(ctx context.Context, alertType string) (int64, error) {
	n, err := r.collection.CountDocuments(ctx, bson.M{"alertType": alertType, "enabled": true})
	if err != nil {
		return 0, fmt.Errorf("count enabled by type: %w", err)
	}
	return n, nil
}

// CountDistinctEnabledUsers [F30-78] 至少有一筆 enabled=true 訂閱的不同使用者數.
func (r *AlertSubRepo) CountDistinctEnabledUsers(ctx context.Context) (int64, error) {
	cursor, err := r.collection.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"enabled": true}}},
		{{Key: "$group", Value: bson.M{"_id": "$userId"}}},
		{{Key: "$count", Value: "n"}},
	})
	if err != nil {
		return 0, fmt.Errorf("count distinct enabled users: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var out []struct {
		N int64 `bson:"n"`
	}
	if err := cursor.All(ctx, &out); err != nil {
		return 0, fmt.Errorf("decode distinct enabled users: %w", err)
	}
	if len(out) == 0 {
		return 0, nil
	}
	return out[0].N, nil
}

// CountWeatherWarningRegions [F30-78] enabled=true 的天氣特報訂閱，依 regions 每個原字串計人數
// （同一筆訂閱內重複的字串只算一次；順序不保證，由呼叫端排序）.
func (r *AlertSubRepo) CountWeatherWarningRegions(ctx context.Context) ([]RegionCount, error) {
	cursor, err := r.collection.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"alertType": TypeWeatherWarning, "enabled": true}}},
		{{Key: "$project", Value: bson.M{"regions": bson.M{"$setUnion": bson.A{bson.M{"$ifNull": bson.A{"$regions", bson.A{}}}, bson.A{}}}}}},
		{{Key: "$unwind", Value: "$regions"}},
		{{Key: "$group", Value: bson.M{"_id": "$regions", "count": bson.M{"$sum": 1}}}},
	})
	if err != nil {
		return nil, fmt.Errorf("count weather warning regions: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []struct {
		Region string `bson:"_id"`
		Count  int64  `bson:"count"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("decode weather warning regions: %w", err)
	}
	out := make([]RegionCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, RegionCount{Region: row.Region, Count: row.Count})
	}
	return out, nil
}

// FindSubscriptionsByUserIDs [F30-78] 一批 userId 的所有訂閱；userIDs 為空時不查資料庫.
func (r *AlertSubRepo) FindSubscriptionsByUserIDs(ctx context.Context, userIDs []string) ([]AlertSubscription, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	cursor, err := r.collection.Find(ctx, bson.M{"userId": bson.M{"$in": userIDs}})
	if err != nil {
		return nil, fmt.Errorf("find subscriptions by userIDs: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []AlertSubscription
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return results, nil
}
