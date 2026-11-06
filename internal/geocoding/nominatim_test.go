package geocoding

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// newTestClientWithURL は httptest.Server の URL を使う NominatimClient を生成する.
// Rate limiter は 100 burst（テストでブロックしない）.
func newTestClientWithURL(server *httptest.Server) *NominatimClient {
	return &NominatimClient{
		httpClient: server.Client(),
		limiter:    rate.NewLimiter(rate.Inf, 100),
		userAgent:  defaultUserAgent,
		baseURL:    server.URL + "/search",
	}
}

// mockNominatimServer は指定した結果を返す httptest.Server を生成する.
func mockNominatimServer(t *testing.T, results []nominatimResult) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

func TestSearch_NormalResults(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "台北",
			DisplayName: "大安區, 台北市, 台灣",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
			Name:        "台北",
			DisplayName: "信義區, 台北市, 台灣",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "信義區",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "台北")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].City != "台北市" {
		t.Errorf("city = %q, want %q", results[0].City, "台北市")
	}

	if results[0].District != "大安區" {
		t.Errorf("district = %q, want %q", results[0].District, "大安區")
	}
}

func TestSearch_EmptyResults(t *testing.T) {
	server := mockNominatimServer(t, []nominatimResult{})
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "nonexistent place xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearch_Deduplication(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "台北市大安區",
			DisplayName: "大安區, 台北市, 台灣 (1)",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
			Name:        "台北市大安區",
			DisplayName: "大安區, 台北市, 台灣 (2)",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
			Name:        "台北市大安區",
			DisplayName: "信義區, 台北市, 台灣",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "信義區",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "台北市大安區")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 deduplicated results, got %d", len(results))
	}

	// First result should be the first occurrence of the duplicate
	if results[0].DisplayName != "大安區, 台北市, 台灣 (1)" {
		t.Errorf("expected first occurrence, got %q", results[0].DisplayName)
	}
}

func TestSearch_CityCountyFallback(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "魚池鄉",
			DisplayName: "魚池鄉, 南投縣, 台灣",
			Address: nominatimAddress{
				County: "臺投縣", // 臺 should be converted to 台
				Town:   "魚池鄉",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "魚池鄉")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// 臺 → 台
	if results[0].City != "台投縣" {
		t.Errorf("city = %q, want %q", results[0].City, "台投縣")
	}
}

func TestSearch_TaiConversion(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "台北",
			DisplayName: "臺北市信義區",
			Address: nominatimAddress{
				City:         "臺北市",
				CityDistrict: "信義區",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "台北")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0].City != "台北市" {
		t.Errorf("city = %q, want %q (臺 should become 台)", results[0].City, "台北市")
	}
}

func TestSearch_DistrictFallback(t *testing.T) {
	tests := []struct {
		name     string
		addr     nominatimAddress
		wantDist string
	}{
		{
			name: "city_district first",
			addr: nominatimAddress{
				City:         "台北市",
				CityDistrict: "中正區",
				Suburb:       "台大",
				Quarter:      "some quarter",
				Town:         "some town",
			},
			wantDist: "中正區",
		},
		{
			name: "suburb when no city_district",
			addr: nominatimAddress{
				City:   "台北市",
				Suburb: "公館",
			},
			wantDist: "公館",
		},
		{
			name: "quarter when no city_district or suburb",
			addr: nominatimAddress{
				City:    "台北市",
				Quarter: "some quarter",
			},
			wantDist: "some quarter",
		},
		{
			name: "town as last resort",
			addr: nominatimAddress{
				County: "南投縣",
				Town:   "魚池鄉",
			},
			wantDist: "魚池鄉",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []nominatimResult{
				{
					Name:        "test",
					DisplayName: "test",
					Address:     tt.addr,
				},
			}

			server := mockNominatimServer(t, raw)
			defer server.Close()

			client := newTestClientWithURL(server)
			results, err := client.Search(context.Background(), "test")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}

			if results[0].District != tt.wantDist {
				t.Errorf("district = %q, want %q", results[0].District, tt.wantDist)
			}
		})
	}
}

func TestSearch_FilterEmptyCityAndDistrict(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "台北",
			DisplayName: "Taiwan",
			Address:     nominatimAddress{}, // both city and district empty
		},
		{
			Name:        "台北",
			DisplayName: "大安區, 台北市",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	client := newTestClientWithURL(server)
	results, err := client.Search(context.Background(), "台北")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the second result should be returned
	if len(results) != 1 {
		t.Errorf("expected 1 result after filtering, got %d", len(results))
	}
}

func TestSearch_RateLimiterBlocks(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "台北",
			DisplayName: "大安區, 台北市",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
	}

	server := mockNominatimServer(t, raw)
	defer server.Close()

	// Use real rate limiter: 1 req/s, burst 1
	client := &NominatimClient{
		httpClient: server.Client(),
		limiter:    rate.NewLimiter(rate.Every(time.Second), 1),
		userAgent:  defaultUserAgent,
		baseURL:    server.URL + "/search",
	}

	// Consume the one available token immediately
	_ = client.limiter.Reserve()

	start := time.Now()

	// Second call should block until token is available (~1s)
	_, err := client.Search(context.Background(), "台北")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have waited at least 800ms (allowing for timing variance)
	if elapsed < 800*time.Millisecond {
		t.Errorf("rate limiter did not block: elapsed = %v, expected >= 800ms", elapsed)
	}
}

// 只打「牛肉麵」時 Nominatim 會找到店名含牛肉麵的餐廳；餐廳與商店不能被當成查天氣的地點.
func TestSearch_SkipsRestaurantsAndShops(t *testing.T) {
	raw := []nominatimResult{
		{
			Name:        "q",
			DisplayName: "林東芳牛肉麵, 中山區, 台北市", Category: "amenity", Type: "restaurant",
			Address: nominatimAddress{City: "台北市", CityDistrict: "中山區"},
		},
		{
			Name:        "q",
			DisplayName: "牛肉麵專賣, 大安區, 台北市", Category: "shop", Type: "deli",
			Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"},
		},
		{
			Name:        "q",
			DisplayName: "台北車站, 中正區, 台北市", Category: "railway", Type: "station",
			Address: nominatimAddress{City: "台北市", CityDistrict: "中正區"},
		},
		{
			Name:        "q",
			DisplayName: "國立臺灣大學, 大安區, 台北市", Category: "amenity", Type: "university",
			Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"},
		},
	}
	server := mockNominatimServer(t, raw)
	defer server.Close()

	results, err := newTestClientWithURL(server).Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].District != "中正區" || results[1].District != "大安區" {
		t.Errorf("want only the station and the university, got %+v", results)
	}
}

// Redis 沒連上時 redisClient 是 nil；以前這裡會 panic，讓整個服務結束.
func TestCachedGeocoderWorksWithoutRedis(t *testing.T) {
	server := mockNominatimServer(t, []nominatimResult{{Name: "大安", DisplayName: "大安區", Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"}}})
	defer server.Close()

	g := NewCachedGeocoder(newTestClientWithURL(server), nil, time.Hour)
	results, err := g.Geocode(context.Background(), "大安")
	if err != nil || len(results) != 1 || results[0].District != "大安區" {
		t.Errorf("results=%+v err=%v", results, err)
	}
}

// osmSportsCentre OSM 的真實 type 值（沿用上游 OSM 的英式拼法）.
const osmSportsCentre = "sports_centre" //nolint:misspell // OSM tag value is the real upstream spelling

// sampleResult 真實 Nominatim 回應（以正式環境相同的查詢參數實抓）的一筆；name、category、type、address 照抄.
func sampleResult(name, category, typ string, addr *nominatimAddress) nominatimResult {
	return nominatimResult{Name: name, DisplayName: name, Category: category, Type: typ, Address: *addr}
}

// TestSearchKeepsOnlyPlacesNamedAsTyped [F26-01] 只留名稱等於使用者打的字的地點.
func TestSearchKeepsOnlyPlacesNamedAsTyped(t *testing.T) {
	taipeiStation := []nominatimResult{
		sampleResult("台北車站", "railway", "station", &nominatimAddress{City: "臺北市", Suburb: "中正區"}),
		sampleResult("臺北車站", "building", "train_station", &nominatimAddress{City: "臺北市", Suburb: "中正區"}),
		sampleResult("台北車站", "building", "transportation", &nominatimAddress{City: "臺北市", Suburb: "中正區"}),
		sampleResult("台北車站", "railway", "stop", &nominatimAddress{City: "臺北市", Suburb: "中正區"}),
	}
	tests := []struct {
		name     string
		query    string
		raw      []nominatimResult
		min, max int
		city     string
		district string
	}{
		{name: "安安 hamlet 暗鞍", query: "安安", max: 0, raw: []nominatimResult{
			sampleResult("暗鞍", "place", "hamlet", &nominatimAddress{City: "新北市", Suburb: "三峽區"}),
		}},
		{name: "嗯嗯 boundaries", query: "嗯嗯", max: 0, raw: []nominatimResult{
			sampleResult("太平島", "place", "islet", &nominatimAddress{City: "高雄市", Suburb: "旗津區"}),
			sampleResult("五股區", "boundary", "administrative", &nominatimAddress{City: "新北市", Suburb: "五股區"}),
			sampleResult("梧棲區", "boundary", "administrative", &nominatimAddress{City: "臺中市", Suburb: "梧棲區"}),
			sampleResult("魚池鄉", "boundary", "administrative", &nominatimAddress{County: "南投縣", Town: "魚池鄉"}),
			sampleResult("五結鄉", "boundary", "administrative", &nominatimAddress{County: "宜蘭縣", Town: "五結鄉"}),
		}},
		{name: "今天 hamlet 金田", query: "今天", max: 0, raw: []nominatimResult{
			sampleResult("金田", "place", "hamlet", &nominatimAddress{County: "花蓮縣", CityDistrict: "大榮里", Town: "鳳林鎮"}),
			sampleResult("金田", "building", "industrial", &nominatimAddress{City: "桃園市", Suburb: "樹林里", Town: "觀音區"}),
		}},
		{name: "運動 mixed", query: "運動", max: 0, raw: []nominatimResult{
			sampleResult("運動部", "office", "government", &nominatimAddress{City: "臺北市", Suburb: "中山區"}),
			sampleResult("苓雅運動園區", "railway", "station", &nominatimAddress{City: "高雄市", Suburb: "苓雅區"}),
			sampleResult("國家運動訓練中心", "leisure", osmSportsCentre, &nominatimAddress{City: "高雄市", Suburb: "左營區"}),
			sampleResult("朝馬國民運動中心", "leisure", osmSportsCentre, &nominatimAddress{City: "臺中市", Suburb: "西屯區"}),
			sampleResult("國立臺灣體育運動大學", "amenity", "university", &nominatimAddress{City: "臺中市", Suburb: "北區"}),
		}},
		{name: "游泳 water", query: "游泳", max: 0, raw: []nominatimResult{
			sampleResult("大城北游泳館", "leisure", "swimming_pool", &nominatimAddress{County: "澎湖縣", CityDistrict: "城北村", Town: "湖西鄉"}),
			sampleResult("游泳池", "natural", "water", &nominatimAddress{City: "臺南市", Suburb: "保安里", Town: "仁德區"}),
			sampleResult("游泳池", "natural", "water", &nominatimAddress{County: "嘉義縣", CityDistrict: "水上村", Town: "水上鄉"}),
			sampleResult("丹鳳公園游泳池", "water", "pond", &nominatimAddress{City: "臺北市", Suburb: "北投區"}),
			sampleResult("台灣國有人造森林永泰林場", "natural", "wood", &nominatimAddress{County: "新竹縣", Town: "五峰鄉"}),
		}},
		{name: "記運動 parks", query: "記運動", max: 0, raw: []nominatimResult{
			sampleResult("二崙運動公園", "leisure", "park", &nominatimAddress{County: "雲林縣", CityDistrict: "崙西村", Town: "二崙鄉"}),
			sampleResult("暖暖運動公園", "leisure", "park", &nominatimAddress{City: "基隆市", Suburb: "暖暖區"}),
			sampleResult("智仁里運動公園", "leisure", "park", &nominatimAddress{City: "基隆市", Suburb: "仁愛區"}),
			sampleResult("新吉工業區運動公園", "leisure", "park", &nominatimAddress{City: "臺南市", Suburb: "新吉里", Town: "安定區"}),
			sampleResult("南昌森林運動公園", "leisure", "park", &nominatimAddress{City: "桃園市", Suburb: "春日里", Town: "桃園區"}),
		}},
		{name: "日月潭", query: "日月潭", min: 1, max: 1, city: "南投縣", district: "日月村", raw: []nominatimResult{
			sampleResult("日月潭", "water", "reservoir", &nominatimAddress{County: "南投縣", CityDistrict: "日月村", Town: "魚池鄉"}),
		}},
		{name: "台北車站", query: "台北車站", min: 1, max: 5, city: "台北市", district: "中正區", raw: taipeiStation},
		{name: "臺北車站 query uses 臺", query: "臺北車站", min: 1, max: 5, city: "台北市", district: "中正區", raw: taipeiStation},
		{name: "旗津 suffix rule", query: "旗津", min: 1, max: 1, city: "高雄市", district: "旗津區", raw: []nominatimResult{
			sampleResult("旗津區", "boundary", "administrative", &nominatimAddress{City: "高雄市", Suburb: "旗津區"}),
		}},
		{name: "台北車站 only 臺北車站 named", query: "台北車站", min: 1, max: 1, city: "台北市", district: "中正區", raw: []nominatimResult{
			sampleResult("臺北車站", "building", "train_station", &nominatimAddress{City: "臺北市", Suburb: "中正區"}),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mockNominatimServer(t, tt.raw)
			defer server.Close()

			results, err := newTestClientWithURL(server).Search(context.Background(), tt.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(results) < tt.min || len(results) > tt.max {
				t.Fatalf("got %d results %+v, want %d..%d", len(results), results, tt.min, tt.max)
			}
			for _, r := range results {
				if r.City != tt.city || r.District != tt.district {
					t.Errorf("got %s/%s, want %s/%s", r.City, r.District, tt.city, tt.district)
				}
			}
		})
	}
}

// TestSearchDropsNonPlaceCategoriesEvenWhenNameMatches [F26-02] 名稱相等仍排除非地點類別.
func TestSearchDropsNonPlaceCategoriesEvenWhenNameMatches(t *testing.T) {
	addr := &nominatimAddress{City: "臺北市", Suburb: "北投區"}
	tests := []struct {
		name  string
		query string
		raw   nominatimResult
		want  int
	}{
		{"leisure park 嗨", "嗨", sampleResult("嗨", "leisure", "park", &nominatimAddress{City: "臺南市", Suburb: "鹽行國中重劃區", Town: "永康區"}), 0},
		{"karaoke_box OK vs ok", "ok", sampleResult("OK", "amenity", "karaoke_box", addr), 0},
		{"shop convenience OK", "ok", sampleResult("OK", "shop", "convenience", addr), 0},
		{"tourism hotel", "某旅館", sampleResult("某旅館", "tourism", "hotel", addr), 0},
		{"landuse residential", "早安再興", sampleResult("早安再興", "landuse", "residential", addr), 0},
		{"place village kept", "某村", sampleResult("某村", "place", "village", addr), 1},
		{"natural peak kept", "某山", sampleResult("某山", "natural", "peak", addr), 1},
		{"railway station kept", "某站", sampleResult("某站", "railway", "station", addr), 1},
		{"landuse retail kept", "某商圈", sampleResult("某商圈", "landuse", "retail", addr), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mockNominatimServer(t, []nominatimResult{tt.raw})
			defer server.Close()

			results, err := newTestClientWithURL(server).Search(context.Background(), tt.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(results) != tt.want {
				t.Errorf("got %d results %+v, want %d", len(results), results, tt.want)
			}
		})
	}
}

// fakeRedis 只實作 GET / SET 的假 Redis（go-redis Hook）；SET 的 []byte 值要存成 string.
type fakeRedis struct{ data map[string]string }

func (f *fakeRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("dial blocked") }
}

func (f *fakeRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		key, _ := args[1].(string)
		switch c := cmd.(type) {
		case *redis.StatusCmd:
			switch v := args[2].(type) {
			case []byte:
				f.data[key] = string(v)
			default:
				f.data[key] = fmt.Sprint(v)
			}
			c.SetVal("OK")
			return nil
		case *redis.StringCmd:
			v, ok := f.data[key]
			if !ok {
				c.SetErr(redis.Nil)
				return redis.Nil
			}
			c.SetVal(v)
			return nil
		}
		return fmt.Errorf("unsupported command %v", args[0])
	}
}

func (f *fakeRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestCachedGeocoderIgnoresOldCacheEntries [F26-03] 舊版快取鍵（geocode:）的未過濾結果不再被讀到.
func TestCachedGeocoderIgnoresOldCacheEntries(t *testing.T) {
	// 兩個舊版寫得出的 key：一般查詢「安安」，以及舊版處理字面輸入「v2:安安」時寫出的 key.
	fake := &fakeRedis{data: map[string]string{
		"geocode:安安":    `[{"City":"新北市","District":"三峽區","DisplayName":"暗鞍"}]`,
		"geocode:v2:安安": `[{"City":"新北市","District":"三峽區","DisplayName":"暗鞍"}]`,
	}}
	if strings.HasPrefix(cacheKeyPrefix, "geocode:") {
		t.Errorf("cacheKeyPrefix %q falls inside the old geocode: key space; old builds could write keys the new build reads", cacheKeyPrefix)
	}
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rc.AddHook(fake)

	server := mockNominatimServer(t, []nominatimResult{
		sampleResult("暗鞍", "place", "hamlet", &nominatimAddress{City: "新北市", Suburb: "三峽區"}),
	})
	defer server.Close()

	g := NewCachedGeocoder(newTestClientWithURL(server), rc, time.Hour)
	results, err := g.Geocode(context.Background(), "安安")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("old cache entry leaked: %+v", results)
	}
	if _, ok := fake.data["geocode2:安安"]; !ok {
		t.Errorf("v2 key not written, keys = %v", fake.data)
	}
}
