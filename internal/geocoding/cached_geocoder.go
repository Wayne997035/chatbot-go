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

// CachedGeocoder は Redis キャッシュを使った Geocoder ラッパー.
type CachedGeocoder struct {
	client      *NominatimClient
	redisClient *redis.Client
	ttl         time.Duration
}

// NewCachedGeocoder は CachedGeocoder のコンストラクタ.
func NewCachedGeocoder(client *NominatimClient, redisClient *redis.Client, ttl time.Duration) *CachedGeocoder {
	return &CachedGeocoder{
		client:      client,
		redisClient: redisClient,
		ttl:         ttl,
	}
}

// Geocode は query に対して Redis キャッシュを確認し、なければ Nominatim API を呼び出す.
func (g *CachedGeocoder) Geocode(ctx context.Context, query string) ([]GeocodeResult, error) {
	normalized := strings.ToLower(strings.TrimSpace(query))
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
