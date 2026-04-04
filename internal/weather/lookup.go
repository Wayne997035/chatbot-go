package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// Lookup 天氣預報查詢（Redis cache-aside + MongoDB）.
type Lookup struct {
	weatherRepo weatherstore.WeatherRepository
	redisClient *redis.Client
	cacheTTL    time.Duration
}

// NewLookup 建立天氣查詢器.
func NewLookup(weatherRepo weatherstore.WeatherRepository, redisClient *redis.Client, cacheTTLSeconds int) *Lookup {
	return &Lookup{
		weatherRepo: weatherRepo,
		redisClient: redisClient,
		cacheTTL:    time.Duration(cacheTTLSeconds) * time.Second,
	}
}

// ByDistrict 依區域名稱查詢天氣（支援多筆結果，如同名區域）.
func (l *Lookup) ByDistrict(ctx context.Context, district string) ([]weatherstore.WeatherForecast, error) {
	// Redis cache check
	if l.redisClient != nil {
		cached, err := l.redisClient.Get(ctx, district).Result()
		if err == nil {
			var forecasts []weatherstore.WeatherForecast
			if json.Unmarshal([]byte(cached), &forecasts) == nil {
				slog.Debug("weather cache hit", "district", district)
				return forecasts, nil
			}
		}
	}

	// MongoDB query
	forecasts, err := l.weatherRepo.FindByDistrict(ctx, district)
	if err != nil {
		return nil, err
	}

	// Cache result
	if l.redisClient != nil && len(forecasts) > 0 {
		if data, err := json.Marshal(forecasts); err == nil {
			l.redisClient.Set(ctx, district, data, l.cacheTTL)
		}
	}

	return forecasts, nil
}

// ByCityAndDistrict 依城市 + 區域精確查詢.
func (l *Lookup) ByCityAndDistrict(ctx context.Context, city, district string) (*weatherstore.WeatherForecast, error) {
	city = strings.ReplaceAll(city, "臺", "台")
	cacheKey := fmt.Sprintf("%s_%s", city, district)

	// Redis cache check
	if l.redisClient != nil {
		cached, err := l.redisClient.Get(ctx, cacheKey).Result()
		if err == nil {
			var forecast weatherstore.WeatherForecast
			if json.Unmarshal([]byte(cached), &forecast) == nil {
				slog.Debug("weather cache hit", "key", cacheKey)
				return &forecast, nil
			}
		}
	}

	// MongoDB query
	forecast, err := l.weatherRepo.FindByDistrictAndCity(ctx, district, city)
	if err != nil {
		return nil, err
	}

	// Cache result
	if l.redisClient != nil && forecast != nil {
		if data, err := json.Marshal(forecast); err == nil {
			l.redisClient.Set(ctx, cacheKey, data, l.cacheTTL)
		}
	}

	return forecast, nil
}
