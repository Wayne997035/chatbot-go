package alert

import (
	"chatbot-go/internal/models"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var alertHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchWeatherWarnings 從 CWA 取得天氣特報 (W-C0033-001).
func FetchWeatherWarnings(ctx context.Context, baseURL, authKey, dataset string) (*models.CWAAlertResponse, error) {
	url := fmt.Sprintf("%s/%s?format=JSON", baseURL, dataset)
	return fetchAndDecode[models.CWAAlertResponse](ctx, url, authKey)
}

// FetchEarthquakes 從 CWA 取得地震報告 (E-A0015-001).
func FetchEarthquakes(ctx context.Context, baseURL, authKey, dataset string) (*models.CWAEarthquakeResponse, error) {
	url := fmt.Sprintf("%s/%s?format=JSON&limit=1", baseURL, dataset)
	return fetchAndDecode[models.CWAEarthquakeResponse](ctx, url, authKey)
}

// FetchTsunamiWarnings 從 CWA 取得海嘯警報 (E-A0014-001).
func FetchTsunamiWarnings(ctx context.Context, baseURL, authKey, dataset string) (*models.CWATsunamiResponse, error) {
	url := fmt.Sprintf("%s/%s?format=JSON", baseURL, dataset)
	return fetchAndDecode[models.CWATsunamiResponse](ctx, url, authKey)
}

// fetchAndDecode 通用 HTTP GET + JSON 解碼，透過泛型消除型別重複.
// authKey 透過 Authorization header 傳遞，避免外洩到 error log.
func fetchAndDecode[T any](ctx context.Context, url, authKey string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", authKey)

	resp, err := alertHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CWA API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &result, nil
}
