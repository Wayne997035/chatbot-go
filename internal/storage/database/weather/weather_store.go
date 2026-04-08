package weatherstore

import (
	"context"
	"log/slog"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeatherRepository 天氣預報儲存庫介面.
type WeatherRepository interface {
	FindByDistrict(ctx context.Context, district string) ([]WeatherForecast, error)
	FindByDistrictAndCity(ctx context.Context, district, city string) (*WeatherForecast, error)
	FindAllDistricts(ctx context.Context) ([]DistrictEntry, error)
	Upsert(ctx context.Context, forecast *WeatherForecast) error
}

// WeatherRepo 天氣預報儲存庫實現.
type WeatherRepo struct {
	collection *mongo.Collection

	// districtCacheMu guards districtOnce reset logic.
	districtCacheMu sync.Mutex
	districtOnce    sync.Once
	districtCache   []DistrictEntry
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

// FindAllDistricts 回傳所有 city+district 組合（in-memory cache, sync.Once）.
// 如果 cache 為空（首次部署尚無資料），允許下次重試.
func (r *WeatherRepo) FindAllDistricts(ctx context.Context) ([]DistrictEntry, error) {
	r.districtCacheMu.Lock()
	// If cache is already populated, return it directly.
	if len(r.districtCache) > 0 {
		cached := r.districtCache
		r.districtCacheMu.Unlock()
		return cached, nil
	}
	r.districtCacheMu.Unlock()

	// Use sync.Once for the actual DB fetch to avoid thundering herd.
	var fetchErr error
	r.districtOnce.Do(func() {
		entries, err := r.fetchAllDistricts(ctx)
		if err != nil {
			fetchErr = err
			return
		}
		// Only cache if non-empty so new deployments without data can retry.
		if len(entries) > 0 {
			r.districtCacheMu.Lock()
			r.districtCache = entries
			r.districtCacheMu.Unlock()
		} else {
			// Reset Once so next call can retry when data arrives.
			r.districtCacheMu.Lock()
			r.districtOnce = sync.Once{}
			r.districtCacheMu.Unlock()
		}
	})

	if fetchErr != nil {
		// Reset Once so caller can retry on error.
		r.districtCacheMu.Lock()
		r.districtOnce = sync.Once{}
		r.districtCacheMu.Unlock()
		return nil, fetchErr
	}

	r.districtCacheMu.Lock()
	cached := r.districtCache
	r.districtCacheMu.Unlock()
	return cached, nil
}

func (r *WeatherRepo) fetchAllDistricts(ctx context.Context) ([]DistrictEntry, error) {
	projection := bson.D{
		{Key: "city", Value: 1},
		{Key: "district", Value: 1},
		{Key: "_id", Value: 0},
	}
	opts := options.Find().SetProjection(projection)

	cursor, err := r.collection.Find(ctx, bson.D{}, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var entries []DistrictEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return nil, err
	}
	return entries, nil
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
