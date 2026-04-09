package geocoding

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const cacheKeyPrefix = "geocode:"

// CachedGeocoder 帶有 Redis cache 的 Geocoder 包裝器.
type CachedGeocoder struct {
	client      *NominatimClient
	redisClient *redis.Client
	ttl         time.Duration
}

// NewCachedGeocoder 建立 CachedGeocoder.
func NewCachedGeocoder(client *NominatimClient, redisClient *redis.Client, ttl time.Duration) *CachedGeocoder {
	return &CachedGeocoder{
		client:      client,
		redisClient: redisClient,
		ttl:         ttl,
	}
}

// Geocode 先查 Redis cache，miss 才呼叫 Nominatim API.
func (g *CachedGeocoder) Geocode(ctx context.Context, query string) ([]GeocodeResult, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(query, "臺", "台")))
	key := cacheKeyPrefix + normalized

	// cache hit
	if cached, err := g.redisClient.Get(ctx, key).Bytes(); err == nil {
		var results []GeocodeResult
		if jsonErr := json.Unmarshal(cached, &results); jsonErr == nil {
			return results, nil
		}
	}

	// cache miss — call Nominatim
	results, err := g.client.Search(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("nominatim search: %w", err)
	}

	// store to cache; Redis 不可用時 fallthrough
	data, marshalErr := json.Marshal(results)
	if marshalErr == nil {
		if setErr := g.redisClient.Set(ctx, key, data, g.ttl).Err(); setErr != nil {
			slog.Warn("geocode cache set failed", "error", setErr)
		}
	}

	return results, nil
}
