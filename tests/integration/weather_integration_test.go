package integration

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// getTestDB 透過 testcontainers 自動啟動 MongoDB container，
// 測試結束後自動清除並停止 container.
func getTestDB(t *testing.T) *mongo.Database {
	t.Helper()
	tm := SetupTestMongoDB(t)
	return tm.Database
}

func TestWeatherRepo_UpsertAndFind(t *testing.T) {
	db := getTestDB(t)
	repo := weatherstore.NewWeatherRepository(db)
	ctx := context.Background()

	forecast := &weatherstore.WeatherForecast{
		ID:       "test_台北市_信義區",
		City:     "台北市",
		District: "信義區",
		Elements: []weatherstore.ForecastElement{
			{ElementName: "T", Description: "平均溫度", Value: "28", StartTime: "2024-01-15T12:00:00+08:00"},
			{ElementName: "RH", Description: "平均相對濕度", Value: "65", StartTime: "2024-01-15T12:00:00+08:00"},
		},
		CreateTime: "2024-01-15 12:00:00",
	}

	// Upsert
	if err := repo.Upsert(ctx, forecast); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// FindByDistrict
	results, err := repo.FindByDistrict(ctx, "信義區")
	if err != nil {
		t.Fatalf("find by district: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].City != "台北市" {
		t.Errorf("city = %q, want 台北市", results[0].City)
	}

	// FindByDistrictAndCity
	result, err := repo.FindByDistrictAndCity(ctx, "信義區", "台北市")
	if err != nil {
		t.Fatalf("find by district and city: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Elements) != 2 {
		t.Errorf("expected 2 elements, got %d", len(result.Elements))
	}

	// Upsert again (update)
	forecast.Elements = append(forecast.Elements, weatherstore.ForecastElement{
		ElementName: "Wx", Description: "天氣現象", Value: "多雲",
	})
	if err := repo.Upsert(ctx, forecast); err != nil {
		t.Fatalf("upsert update: %v", err)
	}

	result, _ = repo.FindByDistrictAndCity(ctx, "信義區", "台北市")
	if len(result.Elements) != 3 {
		t.Errorf("after update: expected 3 elements, got %d", len(result.Elements))
	}

	// FindByDistrictAndCity - not found
	notFound, err := repo.FindByDistrictAndCity(ctx, "中正區", "台北市")
	if err != nil {
		t.Fatalf("find not found: %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for not found")
	}
}

func TestUserRepo_UpsertAndFind(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()

	// 直接用 collection 測試 user upsert 邏輯
	coll := db.Collection("user")

	user := bson.M{
		"_id":        "U1234567890",
		"createTime": time.Now(),
	}

	// Insert
	_, err := coll.InsertOne(ctx, user)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Find
	var found bson.M
	err = coll.FindOne(ctx, bson.M{"_id": "U1234567890"}).Decode(&found)
	if err != nil {
		t.Fatalf("find: %v", err)
	}

	if found["_id"] != "U1234567890" {
		t.Errorf("id = %v, want U1234567890", found["_id"])
	}

	// Upsert (should not fail on duplicate)
	_, err = coll.UpdateOne(ctx, bson.M{"_id": "U1234567890"}, bson.M{"$setOnInsert": user}, options.UpdateOne().SetUpsert(true))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// TestFetchAndStore_Integration 測試完整流程：mock CWA API → FetchAndStore → 驗證 MongoDB 資料.
func TestFetchAndStore_Integration(t *testing.T) {
	db := getTestDB(t)
	repo := weatherstore.NewWeatherRepository(db)
	ctx := context.Background()

	// 建立 mock CWA API server
	now := time.Now().Format(time.RFC3339)
	mockResp := models.CWAResponse{
		Success: "true",
		Records: models.CWARecords{
			Locations: []models.CWALocations{
				{
					LocationsName: "臺北市",
					Location: []models.CWALocation{
						{
							LocationName: "信義區",
							WeatherElement: []models.CWAWeatherElement{
								{
									ElementName: "平均溫度",
									Time: []models.CWATime{
										{
											StartTime:    now,
											ElementValue: []models.CWAElementValue{{Temperature: "28"}},
										},
									},
								},
								{
									ElementName: "平均相對濕度",
									Time: []models.CWATime{
										{
											StartTime:    now,
											ElementValue: []models.CWAElementValue{{RelativeHumidity: "65"}},
										},
									},
								},
								{
									ElementName: "天氣現象",
									Time: []models.CWATime{
										{
											StartTime:    now,
											ElementValue: []models.CWAElementValue{{Weather: "多雲"}},
										},
									},
								},
							},
						},
						{
							LocationName: "中正區",
							WeatherElement: []models.CWAWeatherElement{
								{
									ElementName: "平均溫度",
									Time: []models.CWATime{
										{
											StartTime:    now,
											ElementValue: []models.CWAElementValue{{Temperature: "27"}},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	}))
	defer server.Close()

	// FetchAndStore：只跑 1 次迭代即可（i=1 → F-D0047-001）
	// 用 rateLimitMs=0 加速測試
	err := weather.FetchAndStore(ctx, server.URL, "test-key", 0, repo)
	if err != nil {
		t.Fatalf("FetchAndStore: %v", err)
	}

	// 驗證信義區資料
	result, err := repo.FindByDistrictAndCity(ctx, "信義區", "台北市")
	if err != nil {
		t.Fatalf("find 信義區: %v", err)
	}
	if result == nil {
		t.Fatal("信義區 should have data, got nil")
	}
	if result.City != "台北市" {
		t.Errorf("city = %q, want 台北市", result.City)
	}
	if len(result.Elements) < 3 {
		t.Errorf("expected at least 3 elements (T, RH, Wx), got %d", len(result.Elements))
	}

	// 驗證中正區資料
	result2, err := repo.FindByDistrictAndCity(ctx, "中正區", "台北市")
	if err != nil {
		t.Fatalf("find 中正區: %v", err)
	}
	if result2 == nil {
		t.Fatal("中正區 should have data, got nil")
	}

	// 驗證用 FindByDistrict 查詢（多筆）
	districts, err := repo.FindByDistrict(ctx, "信義區")
	if err != nil {
		t.Fatalf("FindByDistrict: %v", err)
	}
	if len(districts) == 0 {
		t.Error("FindByDistrict should return at least 1 result")
	}

	// 驗證不存在的區域
	notFound, err := repo.FindByDistrictAndCity(ctx, "松山區", "台北市")
	if err != nil {
		t.Fatalf("find 松山區: %v", err)
	}
	if notFound != nil {
		t.Error("松山區 should not have data")
	}
}
