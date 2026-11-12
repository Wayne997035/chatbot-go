package weatherstore

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// LatestCreateTimes 依來源資料集（dataset 欄位）分組，回每組 createTime 的最大值 [F30-52].
// createTime 是固定寬度的 "2006-01-02 15:04:05" 字串，字典序即時間序，所以 $max 即最新.
// 沒有 dataset 欄位的舊文件不參與；空集合回空 map 與 nil.
func (r *WeatherRepo) LatestCreateTimes(ctx context.Context) (map[string]string, error) {
	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "dataset", Value: bson.D{{Key: "$exists", Value: true}, {Key: "$ne", Value: ""}}}}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$dataset"},
			{Key: "latest", Value: bson.D{{Key: "$max", Value: "$createTime"}}},
		}}},
	}
	cursor, err := r.collection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []struct {
		Dataset string `bson:"_id"`
		Latest  string `bson:"latest"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Dataset] = row.Latest
	}
	return out, nil
}
