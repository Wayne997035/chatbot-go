package geocoding

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	// [F24-88] 快取存的是 Nominatim 原始結果（[]nominatimResult），讀出時用現行規則與鄉鎮清單解析；
	// 改解析規則或鄉鎮清單不必換鍵，只有 nominatimResult 欄位結構改變時才升版.
	// [F242-04] 例外：前綴尚未上線前新增欄位不升版（geocode3: 在 PR24 引入，新增 Postcode 時尚未上線，沒有缺欄位的舊資料）；上線後欄位結構一改就升版.
	// [F26-03] 前綴與兩個舊前綴都不重疊：geocode: 存的是未經名稱篩選的結果（例如「安安」→ 三峽區），
	// geocode2: 存的是解析後的 []GeocodeResult（讀成原始結果會靜默變 0 筆）。新前綴不以任一舊前綴開頭，
	// 舊版不論收到什麼輸入都寫不進新版會讀的 key；舊 key 留到 TTL 到期自然消失.
	// [F24-90] PR24 合併 main 時由 geocode:v2: 改為 geocode3:（geocode:v2: 以舊前綴 geocode: 開頭）.
	cacheKeyPrefix = "geocode3:"

	// [F24-44] 本地快取容量與 ttl<=0 時的預設 TTL（localcache.New 對 <=0 會 panic）.
	geocodeLocalMaxSize = 2048
	defaultGeocodeTTL   = 24 * time.Hour

	// [F39-03] 已拿到限速名額的背景請求的硬期限（單次請求逾時 requestTimeout 10 秒 + 重查快取與寫快取的餘裕）；[F39-14] 不含排隊.
	backgroundFetchTimeout = 15 * time.Second
)

// CachedGeocoder 帶有 Redis cache 的 Geocoder 包裝器.
type CachedGeocoder struct {
	client      *NominatimClient
	redisClient *redis.Client
	ttl         time.Duration
	local       *localcache.Cache[[]byte]
	// [F39-03] 同一個 key 併發只發一次 Nominatim 請求（零值可用）.
	flights singleflight.Group
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
			// [F24-90] 快取命中的原始結果同樣套名稱比對與類別排除，舊版寫進快取的不相干地點不會被讀出來.
			return parseResults(raw, query), nil
		}
	}

	// cache miss — call Nominatim
	raw, err := g.fetchAndCache(ctx, key, query)
	if err != nil {
		return nil, fmt.Errorf("nominatim search: %w", err)
	}

	return parseResults(raw, query), nil
}

// fetchAndCache [F39-03] 拿到限速名額後，向 Nominatim 的請求用自己的有界背景期限（WithoutCancel + backgroundFetchTimeout）跑完並寫快取（排隊本身綁呼叫端 ctx，見 [F39-14]），
// 呼叫端 ctx 到期就先回錯誤、背景照跑；同 key 併發只發一次（singleflight）.
// closure 內自己 recover：DoChan 會把 closure 的 panic 在另一條 goroutine 重新丟出，那邊沒有人接，整個服務會結束.
func (g *CachedGeocoder) fetchAndCache(ctx context.Context, key, query string) ([]nominatimResult, error) {
	ch := g.flights.DoChan(key, func() (v any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				// log 只記過 redact 的 panic 值與堆疊，不記使用者原文.
				slog.Error("geocode fetch panicked",
					"panic", redact.Error(fmt.Errorf("%v", rec)), "stack", string(debug.Stack()))
				v, err = nil, fmt.Errorf("geocode fetch panic: %v", rec)
			}
		}()
		// [F39-14] 限速排隊用首位呼叫端自己的 ctx：呼叫端放棄（期限到或取消）就不再排隊，
		// 不會用背景期限替已離開的使用者佔著全服務共用的 Nominatim 限速。預估排不進期限內時 Wait 不預約名額；
		// 提早取消時只有排在最尾端的預約會歸還，所以這裡不保證「放棄就一律歸還名額」.
		// 取捨：同 key 的後到者沿用首位呼叫端的排隊結果，首位放棄或排不到時後到者也拿到錯誤，
		// 即使它自己的預算夠長（窗口 = 該 flight 開著的整段排隊時間）；後到者下一次呼叫開新 flight、用自己的預算重排.
		if waitErr := g.client.wait(ctx); waitErr != nil {
			return nil, waitErr
		}

		// 拿到名額後才脫離呼叫端：重查快取、送請求、寫快取用有界背景期限跑完.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundFetchTimeout)
		defer cancel()

		// flight 外的 cacheGet 與進 flight 之間，前一個 flight 可能剛好寫完快取：先重查，命中就不打 Nominatim.
		if cached, ok := g.cacheGet(bctx, key); ok {
			var hit []nominatimResult
			if json.Unmarshal(cached, &hit) == nil {
				return hit, nil
			}
		}
		raw, fetchErr := g.client.request(bctx, query)
		if fetchErr != nil {
			return nil, fetchErr
		}
		// store raw results to cache；Redis 沒設定或 Set 失敗時寫本地
		if data, marshalErr := json.Marshal(raw); marshalErr == nil {
			g.cacheSet(bctx, key, data)
		}
		return raw, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		raw, _ := res.Val.([]nominatimResult)
		return raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
