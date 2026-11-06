package geocoding

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// [F24-88] 快取存的是 Nominatim 原始結果（[]nominatimResult），讀出時用現行規則與鄉鎮清單解析；
	// 改解析規則或鄉鎮清單不必換鍵，只有 nominatimResult 欄位結構改變時才升版.
	// [F26-03] 前綴與兩個舊前綴都不重疊：geocode: 存的是未經名稱篩選的結果（例如「安安」→ 三峽區），
	// geocode2: 存的是解析後的 []GeocodeResult（讀成原始結果會靜默變 0 筆）。新前綴不以任一舊前綴開頭，
	// 舊版不論收到什麼輸入都寫不進新版會讀的 key；舊 key 留到 TTL 到期自然消失.
	cacheKeyPrefix = "geocode3:"

	// [F24-44] 本地快取容量與 ttl<=0 時的預設 TTL（localcache.New 對 <=0 會 panic）.
	geocodeLocalMaxSize = 2048
	defaultGeocodeTTL   = 24 * time.Hour
)

// CachedGeocoder 帶有 Redis cache 的 Geocoder 包裝器.
type CachedGeocoder struct {
	client      *NominatimClient
	redisClient *redis.Client
	ttl         time.Duration
	local       *localcache.Cache[[]byte]
}

// NewCachedGeocoder 建立 CachedGeocoder.
// Redis 沒設定或出錯時改用本地快取，避免相同 query 重複打 Nominatim.
func NewCachedGeocoder(client *NominatimClient, redisClient *redis.Client, ttl time.Duration) *CachedGeocoder {
	localTTL := ttl
	if localTTL <= 0 {
		localTTL = defaultGeocodeTTL
	}
	return &CachedGeocoder{
		client:      client,
		redisClient: redisClient,
		ttl:         ttl,
		local:       localcache.New[[]byte]("geocode", geocodeLocalMaxSize, localTTL),
	}
}

// Geocode 先查 Redis cache，miss 才呼叫 Nominatim API.
func (g *CachedGeocoder) Geocode(ctx context.Context, query string) ([]GeocodeResult, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(query, "臺", "台")))
	key := cacheKeyPrefix + normalized

	// cache hit（Redis，或 Redis 沒設定 / 出錯時的本地快取）
	if cached, ok := g.cacheGet(ctx, key); ok {
		var raw []nominatimResult
		if jsonErr := json.Unmarshal(cached, &raw); jsonErr == nil {
			return parseResults(raw, query), nil
		}
	}

	// cache miss — call Nominatim
	raw, err := g.client.fetch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("nominatim search: %w", err)
	}

	// store raw results to cache；Redis 沒設定或 Set 失敗時寫本地
	data, marshalErr := json.Marshal(raw)
	if marshalErr == nil {
		g.cacheSet(ctx, key, data)
	}

	return parseResults(raw, query), nil
}

// cacheGet [F24-44] redis.Nil 是權威 miss（不查本地）；Redis 為 nil 或其他錯誤才讀本地.
func (g *CachedGeocoder) cacheGet(ctx context.Context, key string) ([]byte, bool) {
	if g.redisClient != nil {
		cached, err := g.redisClient.Get(ctx, key).Bytes()
		if err == nil {
			return cached, true
		}
		if errors.Is(err, redis.Nil) {
			return nil, false
		}
	}
	return g.local.Get(key)
}

// cacheSet [F24-44] Redis 成功寫入就不寫本地；Redis 為 nil 或 Set 失敗才寫本地.
func (g *CachedGeocoder) cacheSet(ctx context.Context, key string, data []byte) {
	if g.redisClient != nil {
		setErr := g.redisClient.Set(ctx, key, data, g.ttl).Err()
		if setErr == nil {
			return
		}
		slog.Warn("geocode cache write to Redis failed, storing in local cache", "error", redact.Error(setErr))
	}
	g.local.Set(key, data)
}
