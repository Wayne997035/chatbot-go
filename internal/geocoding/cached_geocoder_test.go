package geocoding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（repo 沒有 miniredis）.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialTimeout:   50 * time.Millisecond,
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// countingServer 回傳固定結果並計數請求；status != 200 時回該狀態碼.
func countingServer(t *testing.T, status int, results []nominatimResult) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func daAn() []nominatimResult {
	return []nominatimResult{{DisplayName: "大安區", Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"}}}
}

// geocoderLocalCacheCases 是 Redis nil 與 Redis 出錯共用的本地快取斷言.
func geocoderLocalCacheCases(t *testing.T, client func(t *testing.T) *redis.Client) {
	t.Helper()
	ctx := context.Background()

	t.Run("normalized query shares one request", func(t *testing.T) {
		srv, n := countingServer(t, http.StatusOK, daAn())
		g := NewCachedGeocoder(newTestClientWithURL(srv), client(t), time.Hour)
		for _, q := range []string{"台北車站", " 臺北車站 "} {
			res, err := g.Geocode(ctx, q)
			if err != nil || len(res) != 1 {
				t.Fatalf("query %q: res=%v err=%v", q, res, err)
			}
		}
		if n.Load() != 1 {
			t.Errorf("requests = %d, want 1", n.Load())
		}
	})

	t.Run("different queries do not share a key", func(t *testing.T) {
		srv, n := countingServer(t, http.StatusOK, daAn())
		g := NewCachedGeocoder(newTestClientWithURL(srv), client(t), time.Hour)
		for _, q := range []string{"台北車站", "高雄車站"} {
			if _, err := g.Geocode(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if n.Load() != 2 {
			t.Errorf("requests = %d, want 2", n.Load())
		}
	})

	t.Run("nominatim error not cached", func(t *testing.T) {
		srv, n := countingServer(t, http.StatusInternalServerError, nil)
		g := NewCachedGeocoder(newTestClientWithURL(srv), client(t), time.Hour)
		for i := 0; i < 2; i++ {
			if _, err := g.Geocode(ctx, "台北車站"); err == nil {
				t.Fatalf("call %d: want error", i)
			}
		}
		if n.Load() != 2 {
			t.Errorf("requests = %d, want 2", n.Load())
		}
	})

	t.Run("empty result is cached", func(t *testing.T) {
		srv, n := countingServer(t, http.StatusOK, []nominatimResult{})
		g := NewCachedGeocoder(newTestClientWithURL(srv), client(t), time.Hour)
		for i := 0; i < 2; i++ {
			if _, err := g.Geocode(ctx, "不存在的地方"); err != nil {
				t.Fatal(err)
			}
		}
		if n.Load() != 1 {
			t.Errorf("requests = %d, want 1", n.Load())
		}
	})

	t.Run("non-positive ttl falls back to default", func(t *testing.T) {
		srv, n := countingServer(t, http.StatusOK, daAn())
		g := NewCachedGeocoder(newTestClientWithURL(srv), client(t), 0)
		for i := 0; i < 2; i++ {
			if _, err := g.Geocode(ctx, "台北車站"); err != nil {
				t.Fatal(err)
			}
		}
		if n.Load() != 1 {
			t.Errorf("requests = %d, want 1", n.Load())
		}
	})
}

// [F24-44] Redis 為 nil 時地名解析走本地快取.
func TestGeocoderLocalCacheWhenRedisNil(t *testing.T) {
	geocoderLocalCacheCases(t, func(*testing.T) *redis.Client { return nil })
}

// [F24-44] Redis 出錯時地名解析走本地快取.
func TestGeocoderLocalCacheWhenRedisErrors(t *testing.T) {
	geocoderLocalCacheCases(t, deadRedis)
}
