package weather

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

const (
	// [F24-41] 本地快取容量：全台約 370 區，1024 正常使用不會滿.
	lookupLocalMaxSize = 1024
	// [F24-41] cacheTTLSeconds <= 0 時的預設 TTL（0 會被 localcache 當成非法值）.
	defaultLookupTTL = 10 * time.Minute
)

// Lookup 天氣預報查詢（Redis cache-aside + MongoDB；Redis 沒設定或出錯時改用本地快取）.
type Lookup struct {
	weatherRepo weatherstore.WeatherRepository
	redisClient *redis.Client
	cacheTTL    time.Duration

	// [F24-41] 存 JSON bytes，避免呼叫端共用同一份 slice / pointer.
	localDistrict     *localcache.Cache[[]byte]
	localCityDistrict *localcache.Cache[[]byte]
}

// NewLookup 建立天氣查詢器.
func NewLookup(weatherRepo weatherstore.WeatherRepository, redisClient *redis.Client, cacheTTLSeconds int) *Lookup {
	ttl := time.Duration(cacheTTLSeconds) * time.Second
	localTTL := ttl
	if localTTL <= 0 {
		localTTL = defaultLookupTTL
	}
	return &Lookup{
		weatherRepo:       weatherRepo,
		redisClient:       redisClient,
		cacheTTL:          ttl,
		localDistrict:     localcache.New[[]byte]("weather.district", lookupLocalMaxSize, localTTL),
		localCityDistrict: localcache.New[[]byte]("weather.citydistrict", lookupLocalMaxSize, localTTL),
	}
}

// cacheGet 先讀 Redis；redis.Nil 是權威 miss（不查本地），Redis 為 nil 或其他錯誤才讀本地.
// [F24-41].
func (l *Lookup) cacheGet(ctx context.Context, local *localcache.Cache[[]byte], key string) ([]byte, bool) {
	if l.redisClient != nil {
		cached, err := l.redisClient.Get(ctx, key).Result()
		if err == nil {
			return []byte(cached), true
		}
		if errors.Is(err, redis.Nil) {
			return nil, false
		}
	}
	return local.Get(key)
}

// cacheSet Redis 成功寫入就不寫本地；Redis 為 nil 或 Set 失敗才寫本地.
// [F24-41].
func (l *Lookup) cacheSet(ctx context.Context, local *localcache.Cache[[]byte], key string, data []byte) {
	if l.redisClient != nil {
		if err := l.redisClient.Set(ctx, key, data, l.cacheTTL).Err(); err == nil {
			return
		}
	}
	local.Set(key, data)
}

// ByDistrict 依區域名稱查詢天氣（支援多筆結果，如同名區域）.
func (l *Lookup) ByDistrict(ctx context.Context, district string) ([]weatherstore.WeatherForecast, error) {
	// Cache check（Redis，或 Redis 不可用時的本地快取）
	if cached, ok := l.cacheGet(ctx, l.localDistrict, district); ok {
		var forecasts []weatherstore.WeatherForecast
		if json.Unmarshal(cached, &forecasts) == nil {
			slog.Debug("weather cache hit", "district", district)
			return forecasts, nil
		}
	}

	// MongoDB query
	forecasts, err := l.weatherRepo.FindByDistrict(ctx, district)
	if err != nil {
		return nil, err
	}

	// Cache result
	if len(forecasts) > 0 {
		if data, err := json.Marshal(forecasts); err == nil {
			l.cacheSet(ctx, l.localDistrict, district, data)
		}
	}

	return forecasts, nil
}

// ByCityAndDistrict 依城市 + 區域精確查詢.
func (l *Lookup) ByCityAndDistrict(ctx context.Context, city, district string) (*weatherstore.WeatherForecast, error) {
	city = strings.ReplaceAll(city, "臺", "台")
	cacheKey := fmt.Sprintf("%s_%s", city, district)

	// Cache check（Redis，或 Redis 不可用時的本地快取）
	if cached, ok := l.cacheGet(ctx, l.localCityDistrict, cacheKey); ok {
		var forecast weatherstore.WeatherForecast
		if json.Unmarshal(cached, &forecast) == nil {
			slog.Debug("weather cache hit", "key", cacheKey)
			return &forecast, nil
		}
	}

	// MongoDB query
	forecast, err := l.weatherRepo.FindByDistrictAndCity(ctx, district, city)
	if err != nil {
		return nil, err
	}

	// Cache result
	if forecast != nil {
		if data, err := json.Marshal(forecast); err == nil {
			l.cacheSet(ctx, l.localCityDistrict, cacheKey, data)
		}
	}

	return forecast, nil
}
