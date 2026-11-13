package profile

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CountFilledProfiles [F30-78] 至少填過一個欄位的身體資料數（只含 updateTime 的空殼不算）.
func (r *ProfileRepo) CountFilledProfiles(ctx context.Context) (int64, error) {
	filter := bson.M{"$or": bson.A{
		bson.M{"weightKg": bson.M{"$gt": 0}},
		bson.M{"heightCm": bson.M{"$gt": 0}},
		bson.M{"age": bson.M{"$gt": 0}},
		bson.M{"sex": bson.M{"$nin": bson.A{"", nil}}},
	}}
	n, err := r.collection.CountDocuments(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("count filled profiles: %w", err)
	}
	return n, nil
}
