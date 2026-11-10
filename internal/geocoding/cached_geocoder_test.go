package geocoding

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（連線錯誤）.
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
	return []nominatimResult{{Name: "台北車站", DisplayName: "大安區", Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"}}}
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

// fakeRedisHook [F241-04] 以 go-redis Hook 實作的記憶體假 Redis：不連線，SET/GET 走 map.
type fakeRedisHook struct {
	mu   sync.Mutex
	data map[string]string
}

func (h *fakeRedisHook) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("fake redis: no network")
	}
}

func (h *fakeRedisHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		args := cmd.Args()
		switch strings.ToLower(cmd.Name()) {
		case "set":
			key, _ := args[1].(string)
			switch v := args[2].(type) {
			case []byte: // go-redis 對 []byte 值傳 []byte，必須轉 string 存
				h.data[key] = string(v)
			case string:
				h.data[key] = v
			default:
				return errors.New("fake redis: unsupported SET value type")
			}
			if sc, ok := cmd.(*redis.StatusCmd); ok {
				sc.SetVal("OK")
			}
			return nil
		case "get":
			key, _ := args[1].(string)
			v, ok := h.data[key]
			sc, isStr := cmd.(*redis.StringCmd)
			if !ok || !isStr {
				cmd.SetErr(redis.Nil)
				return redis.Nil
			}
			sc.SetVal(v)
			return nil
		}
		return errors.New("fake redis: unsupported command " + cmd.Name())
	}
}

func (h *fakeRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestCachedGeocoderRedisRoundTrip [F241-04] 經 Redis 存取的那一份與本地解析結果一致，且命中時不打 Nominatim.
func TestCachedGeocoderRedisRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, n := countingServer(t, http.StatusOK, riyuetan())
	hook := &fakeRedisHook{data: map[string]string{}}
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rc.AddHook(hook)
	t.Cleanup(func() { _ = rc.Close() })

	g := NewCachedGeocoder(newTestClientWithURL(srv), rc, time.Hour)
	check := func(label string) {
		t.Helper()
		res, err := g.Geocode(ctx, "日月潭")
		if err != nil {
			t.Fatalf("%s: Geocode: %v", label, err)
		}
		if len(res) != 1 || res[0].City != "南投縣" || res[0].District != "魚池鄉" {
			t.Fatalf("%s: results = %+v, want [(南投縣, 魚池鄉)]", label, res)
		}
	}

	check("first call")
	hook.mu.Lock()
	keys := 0
	for k := range hook.data {
		if strings.HasPrefix(k, "geocode3:") {
			keys++
		}
	}
	hook.mu.Unlock()
	if keys != 1 {
		t.Fatalf("redis geocode3: keys = %d, want 1", keys)
	}

	// 換掉本地快取，第二次只能靠 Redis.
	g.local = localcache.New[[]byte]("geocode-test", 10, time.Hour)
	check("second call via redis")
	if got := n.Load(); got != 1 {
		t.Errorf("nominatim calls = %d, want 1", got)
	}
}
