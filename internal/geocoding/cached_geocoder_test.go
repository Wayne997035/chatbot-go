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

// [F39-03] 呼叫端逾時回錯誤後，背景請求仍跑完並寫快取；之後的呼叫走快取、不再打 Nominatim.
func TestGeocodeTimeoutStillFillsCache(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(daAn()); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	g := NewCachedGeocoder(newTestClientWithURL(srv), nil, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := g.Geocode(ctx, "台北車站")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call err = %v, want DeadlineExceeded", err)
	}

	// 不用第二次 Geocode 當同步手段：輪詢快取直到背景寫入.
	key := cacheKeyPrefix + "台北車站"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := g.cacheGet(context.Background(), key); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background fetch did not fill the cache within 2s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, err := g.Geocode(context.Background(), "台北車站")
	if err != nil || len(res) == 0 {
		t.Fatalf("second call res=%v err=%v", res, err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("server requests = %d, want 1", got)
	}
}

// [F39-03] 同一 query 同時 5 個呼叫端只發一次請求；伺服器卡住直到收到 5 個請求或 2 秒，
// 沒有 singleflight 時 5 個請求會在放行前全部送出.
func TestGeocodeConcurrentSameQueryFetchesOnce(t *testing.T) {
	const callers = 5
	var n atomic.Int32
	allArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == callers {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-time.After(2 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(daAn()); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	g := NewCachedGeocoder(newTestClientWithURL(srv), nil, time.Hour)

	var wg sync.WaitGroup
	errs := make([]error, callers)
	lens := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := g.Geocode(context.Background(), "台北車站")
			errs[i], lens[i] = err, len(res)
		}(i)
	}
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil || lens[i] == 0 {
			t.Errorf("caller %d: len=%d err=%v", i, lens[i], errs[i])
		}
	}
	if got := n.Load(); got != 1 {
		t.Errorf("server requests = %d, want 1", got)
	}
}

// [F39-03] fetch 內 panic（client 為 nil → limiter.Wait 對 nil 解參考）只轉成錯誤，不殺程序，flight 清除後可再呼叫.
func TestGeocodePanicInFlightReturnsError(t *testing.T) {
	g := NewCachedGeocoder(nil, nil, time.Hour)
	for i := 0; i < 2; i++ {
		res, err := g.Geocode(context.Background(), "台北車站")
		if err == nil {
			t.Fatalf("call %d: err = nil, res = %v", i, res)
		}
	}
}

// [F39-14] 呼叫端已放棄（期限到）的地名查詢不得佔著全服務共用的 Nominatim 限速：
// 12 個不同查詢各帶 1.5 秒期限擠進佇列後，第 13 個 10 秒期限的查詢必須在 2.6 秒內成功、伺服器只收到 ≤3 個請求.
// 限速用 NewNominatimClient 建出的真限速（每秒 1、burst 1）；newTestClientWithURL 的限速是 rate.Inf，不排隊，測不到.
func TestGeocodeAbandonedLookupsDoNotHoldRateLimit(t *testing.T) {
	var reqs, active atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		reqs.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(daAn()); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := NewNominatimClient()
	client.baseURL = srv.URL + "/search"
	client.httpClient = srv.Client()
	g := NewCachedGeocoder(client, nil, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_, _ = g.Geocode(ctx, "放棄查詢"+string(rune('A'+i)))
		}(i)
	}

	// 等第 1 個請求到伺服器（初始名額已被用掉），再多等 100ms 讓 12 個都進佇列，才送受害者.
	pollDeadline := time.Now().Add(3 * time.Second)
	for reqs.Load() < 1 {
		if time.Now().After(pollDeadline) {
			t.Fatal("server never received the first request")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := g.Geocode(ctx, "受害查詢")
	elapsed, got := time.Since(start), reqs.Load()
	if err != nil || len(res) == 0 {
		t.Errorf("victim lookup res=%v err=%v elapsed=%v reqs=%d", res, err, elapsed, got)
	}
	if elapsed >= 2600*time.Millisecond {
		t.Errorf("victim lookup took %v, want < 2.6s: abandoned lookups still hold the limiter", elapsed)
	}
	if got > 3 {
		t.Errorf("server requests = %d when victim returned, want <= 3", got)
	}

	// 不讓背景 goroutine 洩到下一支測試：等放棄的呼叫端都返回、伺服器沒有進行中的請求，再留一點時間給快取寫入.
	wg.Wait()
	settle := time.Now().Add(3 * time.Second)
	for active.Load() > 0 && time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}
