package geocoding

import (
	"chatbot-go/internal/region"
	"context"
	"encoding/json"
	"errors"
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
	Postcode     string `json:"postcode"`
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
	raw, err := c.fetch(ctx, query)
	if err != nil {
		return nil, err
	}

	return parseResults(raw, query), nil
}

// withoutURL [F38-06] *url.Error 的字串帶完整請求 URL，q= 是使用者打的原文；只留底層原因（逾時、連線被拒），
// 任何呼叫端記 log 都不會帶出原文.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// fetch [F24-88] 對 query 呼叫 Nominatim API，回傳未解析的原始結果（快取存的就是這份）.
func (c *NominatimClient) fetch(ctx context.Context, query string) ([]nominatimResult, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.request(ctx, query)
}

// wait [F39-14] 限速排隊，與 request 拆開才能分別給 ctx：排隊用呼叫端自己的 ctx，請求才可以脫離呼叫端.
// 預估等待超過 ctx 期限時 Limiter.Wait 直接回錯、不預約名額；ctx 在排到之前被取消時只有排在最尾端的預約會歸還.
func (c *NominatimClient) wait(ctx context.Context) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}
	return nil
}

// request [F39-14] 建請求、送出、解析回應；不排隊（呼叫前須已通過 wait）.
func (c *NominatimClient) request(ctx context.Context, query string) ([]nominatimResult, error) {
	apiURL := c.buildURL(query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", withoutURL(err))
	}

	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", withoutURL(err))
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

	return raw, nil
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
	seen := make(map[string]int)
	results := make([]GeocodeResult, 0, len(raw))

	for i := range raw {
		if isNonPlace(&raw[i]) || !nameMatchesQuery(raw[i].Name, query) {
			continue
		}
		city := resolveCity(&raw[i].Address)
		district := resolveDistrict(&raw[i].Address, city)
		if district == "" {
			district = districtByPostcode(&raw[i].Address, city)
		}

		// [F24-88] 查不到縣市或鄉鎮的結果查不到天氣，不列入.
		if city == "" || district == "" {
			continue
		}

		// [F241-14] 去重後只來自公車站的結果要使用者確認；同一個縣市鄉鎮再出現非公車站結果就不用（不論先後）.
		busStop := isBusStop(&raw[i])
		key := city + "|" + district
		if idx, ok := seen[key]; ok {
			// [F38-01] 同一個鄉鎮只要有一筆區域或景點類結果就算（不論先後）.
			if isAreaResult(&raw[i]) {
				results[idx].Area = true
			}
			// [F242-03] 目前這個鄉鎮只來自公車站（NeedsConfirm 仍為 true）時，第一個一般地點換掉公車站：
			// 不用再確認，卡片標籤也改顯示一般地點的名稱（按鈕送出的縣市+鄉鎮不變）.
			if !busStop && results[idx].NeedsConfirm {
				results[idx].NeedsConfirm = false
				results[idx].DisplayName = raw[i].DisplayName
			}
			continue
		}

		seen[key] = len(results)
		results = append(results, GeocodeResult{
			City:         city,
			District:     district,
			DisplayName:  raw[i].DisplayName,
			NeedsConfirm: busStop,
			Area:         isAreaResult(&raw[i]),
		})
	}

	return results
}

// areaCategories [F38-01][F38-05] 代表一個區域或景點的 OSM category（允許清單）：聚落、行政區、山、水、古蹟.
// 用允許清單而不是排除清單：品牌與一般詞常以 landuse（麥當勞、鴻海）、building（辦公室、健身房）、office（遠傳）出現，
// 排除清單遇到沒列到的類別就會讓它們單打跳出天氣卡；允許清單漏掉的只是「少出卡」，使用者照舊可打「X天氣」.
var areaCategories = map[string]bool{
	"place": true, "boundary": true, "natural": true, "water": true, "waterway": true, "historic": true,
}

// areaTourismTypes [F38-05] tourism 只認景點類型；hotel、apartment、artwork（「家」「老街」）等不算.
var areaTourismTypes = map[string]bool{
	"attraction": true, "museum": true, "viewpoint": true, "theme_park": true, "zoo": true, "aquarium": true,
}

// isAreaResult [F38-01][F38-05] 這筆結果是區域或景點類（天母 place、日月潭 water、士林夜市 tourism/attraction），
// 不是車站、站牌、商家、建築或用地.
func isAreaResult(r *nominatimResult) bool {
	return areaCategories[r.Category] || (r.Category == "tourism" && areaTourismTypes[r.Type])
}

// isBusStop [F241-14] OSM 公車站（highway/bus_stop）：可能是遠處同名站牌，證據比一般地點弱.
func isBusStop(r *nominatimResult) bool {
	return r.Category == "highway" && r.Type == "bus_stop"
}

// resolveCity [F24-88] 依序看 City、County，取第一個屬於 22 縣市清單的值（臺 → 台）；都不是回 "".
func resolveCity(addr *nominatimAddress) string {
	for _, v := range []string{addr.City, addr.County} {
		if c := replaceTai(v); region.IsCounty(c) {
			return c
		}
	}

	return ""
}

// resolveDistrict [F24-88] 依序看 CityDistrict、Suburb、Quarter、Town、City，
// 取第一個屬於該縣市鄉鎮清單的值（臺 → 台）；都不是或 county 為空回 "".
func resolveDistrict(addr *nominatimAddress, county string) string {
	if county == "" {
		return ""
	}

	for _, v := range []string{addr.CityDistrict, addr.Suburb, addr.Quarter, addr.Town, addr.City} {
		if d := replaceTai(v); region.IsTownship(county, d) {
			return d
		}
	}

	return ""
}

// districtByPostcode [F241-13] 地址沒有鄉鎮欄位（例：九份 {village: 九份, city: 新北市, postcode: 22448}）時，
// 用郵遞區號前 3 碼補鄉鎮；查不到唯一一區回 "".
// 郵遞區號對到的縣市必須與已解析的縣市相同：兩者不同代表 Nominatim 的縣市或郵遞區號有一邊錯，不猜；
// 縣市為空時沒有可交叉檢查的對象，郵遞區號單獨不夠可信，同樣不補.
func districtByPostcode(addr *nominatimAddress, city string) string {
	if city == "" {
		return ""
	}

	county, town, ok := region.TownshipByPostcode(addr.Postcode)
	if !ok || county != city {
		return ""
	}

	return town
}

// replaceTai replaces 臺 with 台.
func replaceTai(s string) string {
	return strings.ReplaceAll(s, "臺", "台")
}
