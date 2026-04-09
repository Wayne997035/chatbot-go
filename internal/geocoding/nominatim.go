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

// nominatimAddress は Nominatim address フィールドの構造体.
type nominatimAddress struct {
	City         string `json:"city"`
	County       string `json:"county"`
	CityDistrict string `json:"city_district"`
	Suburb       string `json:"suburb"`
	Quarter      string `json:"quarter"`
	Town         string `json:"town"`
}

// nominatimResult は Nominatim API の1件のレスポンス構造体.
type nominatimResult struct {
	DisplayName string           `json:"display_name"`
	Address     nominatimAddress `json:"address"`
}

// NominatimClient は Nominatim geocoding API のクライアント.
type NominatimClient struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	userAgent  string
	baseURL    string
}

// NewNominatimClient は NominatimClient のコンストラクタ.
func NewNominatimClient() *NominatimClient {
	return &NominatimClient{
		httpClient: &http.Client{Timeout: requestTimeout},
		limiter:    rate.NewLimiter(rate.Every(time.Second), 1),
		userAgent:  defaultUserAgent,
		baseURL:    nominatimDefaultBaseURL,
	}
}

// Search は query に対して Nominatim API を呼び出し、GeocodeResult のスライスを返す.
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var raw []nominatimResult
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return parseResults(raw), nil
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
func parseResults(raw []nominatimResult) []GeocodeResult {
	seen := make(map[string]struct{})
	results := make([]GeocodeResult, 0, len(raw))

	for i := range raw {
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
