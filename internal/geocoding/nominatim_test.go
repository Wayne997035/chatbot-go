package geocoding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
			DisplayName: "大安區, 台北市, 台灣",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
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
			DisplayName: "大安區, 台北市, 台灣 (1)",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
			DisplayName: "大安區, 台北市, 台灣 (2)",
			Address: nominatimAddress{
				City:         "台北市",
				CityDistrict: "大安區",
			},
		},
		{
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
			DisplayName: "Taiwan",
			Address:     nominatimAddress{}, // both city and district empty
		},
		{
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
			DisplayName: "林東芳牛肉麵, 中山區, 台北市", Category: "amenity", Type: "restaurant",
			Address: nominatimAddress{City: "台北市", CityDistrict: "中山區"},
		},
		{
			DisplayName: "牛肉麵專賣, 大安區, 台北市", Category: "shop", Type: "deli",
			Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"},
		},
		{
			DisplayName: "台北車站, 中正區, 台北市", Category: "railway", Type: "station",
			Address: nominatimAddress{City: "台北市", CityDistrict: "中正區"},
		},
		{
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
	server := mockNominatimServer(t, []nominatimResult{{DisplayName: "大安區", Address: nominatimAddress{City: "台北市", CityDistrict: "大安區"}}})
	defer server.Close()

	g := NewCachedGeocoder(newTestClientWithURL(server), nil, time.Hour)
	results, err := g.Geocode(context.Background(), "大安")
	if err != nil || len(results) != 1 || results[0].District != "大安區" {
		t.Errorf("results=%+v err=%v", results, err)
	}
}
