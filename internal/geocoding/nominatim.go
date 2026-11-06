package geocoding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	nominatimDefaultBaseURL = "https://nominatim.openstreetmap.org/search"
	defaultUserAgent        = "chatbot-go/1.0 (github.com/Wayne997035/chatbot-go)"
	requestTimeout          = 10 * time.Second
)

// nominatimAddress Nominatim address 欄位結構.
type nominatimAddress struct {
	City         string `json:"city"`
	County       string `json:"county"`
	CityDistrict string `json:"city_district"`
	Suburb       string `json:"suburb"`
	Quarter      string `json:"quarter"`
	Town         string `json:"town"`
}

// nominatimResult Nominatim API 單筆回應結構（format=jsonv2）.
type nominatimResult struct {
	Name        string           `json:"name"`
	DisplayName string           `json:"display_name"`
	Category    string           `json:"category"`
	Type        string           `json:"type"`
	Address     nominatimAddress `json:"address"`
}

// foodAmenities 餐飲與娛樂類 amenity；店名常含菜名（例如「林東芳牛肉麵」），不能當成查天氣的地點.
var foodAmenities = map[string]bool{
	"restaurant": true, "fast_food": true, "cafe": true, "food_court": true,
	"bar": true, "pub": true, "ice_cream": true, "biergarten": true, "karaoke_box": true,
}

// nonPlaceTypes 整個 category 都不是地點的（shop、leisure），以及 category 底下不是地點的 type.
var (
	nonPlaceCategories = map[string]bool{"shop": true, "leisure": true}
	nonPlaceTypes      = map[string]map[string]bool{
		"amenity": foodAmenities,
		"tourism": {
			"hotel": true, "guest_house": true, "hostel": true, "motel": true,
			"apartment": true, "chalet": true, "information": true,
		},
		"landuse": {"residential": true, "construction": true},
	}
)

// adminSuffixes 行政區字尾；使用者打「旗津」、Nominatim 名稱是「旗津區」時視為同一個地點.
var adminSuffixes = []string{"市", "縣", "區", "鄉", "鎮", "村", "里"}

// isNonPlace [F26-02] 商店、餐廳、公園、旅館、住宅區等不是使用者要查天氣的地點；車站、景點、行政區等仍保留.
func isNonPlace(r *nominatimResult) bool {
	return nonPlaceCategories[r.Category] || nonPlaceTypes[r.Category][r.Type]
}

// normalizeName 名稱與查詢字共用的正規化：臺→台、去頭尾空白、不分大小寫.
func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(replaceTai(s)))
}

// nameMatchesQuery [F26-01] 結果名稱等於使用者打的字（或去掉一個行政區字尾後相等）.
// Nominatim 是模糊搜尋，「安安」會回名叫「暗鞍」的聚落；只有名稱真的等於查詢字，才當成使用者在說這個地名.
func nameMatchesQuery(name, query string) bool {
	n, q := normalizeName(name), normalizeName(query)
	if n == "" || q == "" {
		return false
	}
	if n == q {
		return true
	}
	for _, suffix := range adminSuffixes {
		if strings.HasSuffix(n, suffix) && strings.TrimSuffix(n, suffix) == q {
			return true
		}
	}

	return false
}

// NominatimClient Nominatim geocoding API 客戶端.
type NominatimClient struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	userAgent  string
	baseURL    string
}

// NewNominatimClient 建立 NominatimClient.
func NewNominatimClient() *NominatimClient {
	return &NominatimClient{
		httpClient: &http.Client{Timeout: requestTimeout},
		limiter:    rate.NewLimiter(rate.Every(time.Second), 1),
		userAgent:  defaultUserAgent,
		baseURL:    nominatimDefaultBaseURL,
	}
}

// Search 對 query 呼叫 Nominatim API，回傳 GeocodeResult slice.
func (c *NominatimClient) Search(ctx context.Context, query string) ([]GeocodeResult, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	apiURL := c.buildURL(query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nominatim API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024)) // 512 KB: 5 results can't exceed this
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var raw []nominatimResult
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return parseResults(raw, query), nil
}

// buildURL constructs the Nominatim search URL for a given query.
func (c *NominatimClient) buildURL(query string) string {
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "jsonv2")
	params.Set("addressdetails", "1")
	params.Set("countrycodes", "tw")
	params.Set("limit", "5")
	params.Set("accept-language", "zh-TW")

	return c.baseURL + "?" + params.Encode()
}

// parseResults converts raw Nominatim results into GeocodeResult, deduplicating by city+district.
// 只留名稱等於 query 的地點 [F26-01]，並排除非地點類別 [F26-02].
func parseResults(raw []nominatimResult, query string) []GeocodeResult {
	seen := make(map[string]struct{})
	results := make([]GeocodeResult, 0, len(raw))

	for i := range raw {
		if isNonPlace(&raw[i]) || !nameMatchesQuery(raw[i].Name, query) {
			continue
		}
		city := resolveCity(&raw[i].Address)
		district := resolveDistrict(&raw[i].Address)

		if city == "" && district == "" {
			continue
		}

		key := city + "|" + district
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		results = append(results, GeocodeResult{
			City:        city,
			District:    district,
			DisplayName: raw[i].DisplayName,
		})
	}

	return results
}

// resolveCity 決定 city 欄位，臺 → 台.
func resolveCity(addr *nominatimAddress) string {
	if addr.City != "" {
		return replaceTai(addr.City)
	}

	if addr.County != "" {
		return replaceTai(addr.County)
	}

	return ""
}

// resolveDistrict 決定 district 欄位、優先順序：city_district → suburb → quarter → town.
func resolveDistrict(addr *nominatimAddress) string {
	switch {
	case addr.CityDistrict != "":
		return addr.CityDistrict
	case addr.Suburb != "":
		return addr.Suburb
	case addr.Quarter != "":
		return addr.Quarter
	case addr.Town != "":
		return addr.Town
	default:
		return ""
	}
}

// replaceTai replaces 臺 with 台.
func replaceTai(s string) string {
	return strings.ReplaceAll(s, "臺", "台")
}
