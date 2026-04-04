package weatherstore

import (
	"context"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeatherRepository 天氣預報儲存庫介面.
type WeatherRepository interface {
	FindByDistrict(ctx context.Context, district string) ([]WeatherForecast, error)
	FindByDistrictAndCity(ctx context.Context, district, city string) (*WeatherForecast, error)
	Upsert(ctx context.Context, forecast *WeatherForecast) error
}

// WeatherRepo 天氣預報儲存庫實現.
type WeatherRepo struct {
	collection *mongo.Collection
}

// NewWeatherRepository 建立天氣預報儲存庫.
func NewWeatherRepository(db *mongo.Database) *WeatherRepo {
	ctx := context.Background()
	coll := db.Collection("weatherForecast")

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "district", Value: 1}}},
		{Keys: bson.D{{Key: "district", Value: 1}, {Key: "city", Value: 1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		slog.Warn("weather index creation", "error", err)
	}

	return &WeatherRepo{collection: coll}
}

func (r *WeatherRepo) FindByDistrict(ctx context.Context, district string) ([]WeatherForecast, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"district": district})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var results []WeatherForecast
	if err := cursor.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (r *WeatherRepo) FindByDistrictAndCity(ctx context.Context, district, city string) (*WeatherForecast, error) {
	var result WeatherForecast
	err := r.collection.FindOne(ctx, bson.M{"district": district, "city": city}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return &result, nil
}

func (r *WeatherRepo) Upsert(ctx context.Context, forecast *WeatherForecast) error {
	filter := bson.M{"district": forecast.District, "city": forecast.City}
	update := bson.M{"$set": forecast}
	opts := options.UpdateOne().SetUpsert(true)

	_, err := r.collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		slog.Error("weather upsert failed", "city", forecast.City, "district", forecast.District, "error", err)
		return err
	}
	return nil
}
