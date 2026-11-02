package weather

import (
	"chatbot-go/internal/models"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// elementNameMap CWA 中文元素名稱 → 縮寫對照.
var elementNameMap = map[string]string{
	"平均溫度":     "T",
	"平均露點溫度":   "Td",
	"平均相對濕度":   "RH",
	"風速":       "WS",
	"風向":       "WD",
	"12小時降雨機率": "PoP12h",
	"6小時降雨機率":  "PoP6h",
	"天氣現象":     "Wx",
	"天氣預報綜合描述": "WeatherDescription",
	"紫外線指數":    "UVI", // 以前誤對到舒適度指數（CI），紫外線永遠讀不到
	"體感溫度":     "AT",
}

var apiClient = &http.Client{Timeout: 30 * time.Second}

// FetchAndStore 從 CWA Open Data API 取得天氣資料並存入 MongoDB.
func FetchAndStore(ctx context.Context, baseURL, authKey string, rateLimitMs int, repo weatherstore.WeatherRepository) error {
	// F-D0047-001 ~ F-D0047-089（奇數），對應全台 22 縣市
	var total, failed int
	var lastErr error
	for i := 1; i <= 89; i += 2 {
		datasetID := fmt.Sprintf("F-D0047-%03d", i)
		url := fmt.Sprintf("%s/%s?format=JSON", strings.TrimRight(baseURL, "/"), datasetID)

		total++
		if err := fetchAndProcess(ctx, url, authKey, repo); err != nil {
			failed++
			lastErr = err
			slog.Warn("fetch weather data", "dataset", datasetID, "error", err)
			// 繼續處理其他區域，不中斷
		}

		// Rate limiting
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(rateLimitMs) * time.Millisecond):
		}
	}

	// 以前全部失敗（例如授權碼錯誤回 401）也回傳成功，log 會寫「同步完成」.
	if failed == total {
		return fmt.Errorf("all %d weather datasets failed, last error: %w", total, lastErr)
	}
	if failed > 0 {
		slog.Warn("weather sync partially failed", "failed", failed, "total", total)
	}
	return nil
}

func fetchAndProcess(ctx context.Context, url, authKey string, repo weatherstore.WeatherRepository) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", authKey)

	resp, err := apiClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CWA API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var cwaResp models.CWAResponse
	if err := json.Unmarshal(body, &cwaResp); err != nil {
		return fmt.Errorf("parse CWA response: %w", err)
	}

	now := time.Now()
	createTime := now.Format("2006-01-02 15:04:05")

	for _, locations := range cwaResp.Records.Locations {
		cityName := strings.ReplaceAll(locations.LocationsName, "臺", "台")

		for _, loc := range locations.Location {
			elements := extractElements(loc.WeatherElement, now)
			if len(elements) == 0 {
				continue
			}

			forecast := &weatherstore.WeatherForecast{
				ID:         fmt.Sprintf("%s_%s", cityName, loc.LocationName),
				City:       cityName,
				District:   loc.LocationName,
				Elements:   elements,
				CreateTime: createTime,
			}

			if err := repo.Upsert(ctx, forecast); err != nil {
				slog.Warn("upsert weather", "city", cityName, "district", loc.LocationName, "error", err)
			}
		}
	}

	return nil
}

func extractElements(weatherElements []models.CWAWeatherElement, now time.Time) []weatherstore.ForecastElement {
	var elements []weatherstore.ForecastElement

	for _, we := range weatherElements {
		abbr, ok := elementNameMap[we.ElementName]
		if !ok {
			continue
		}

		closest := findClosestTime(we.Time, now)
		if closest == nil {
			continue
		}

		value := extractValue(abbr, closest.ElementValue)
		if value == "" {
			continue
		}

		elements = append(elements, weatherstore.ForecastElement{
			ElementName: abbr,
			Description: we.ElementName,
			Value:       value,
			StartTime:   closest.StartTime,
		})
	}

	return elements
}

// findClosestTime 選要顯示的時段：優先選「現在所在」的時段（StartTime ≤ now < EndTime）.
// 以前只看開始時間離現在多近：12 小時一段時，下午 1 點會選到晚上 6 點開始的那段.
// 沒有結束時間的資料（只有時間點）才選最接近的.
func findClosestTime(times []models.CWATime, now time.Time) *models.CWATime {
	if len(times) == 0 {
		return nil
	}
	for i := range times {
		start, end := parseTimeSafe(times[i].StartTime), parseTimeSafe(times[i].EndTime)
		if !start.IsZero() && !end.IsZero() && !now.Before(start) && now.Before(end) {
			return &times[i]
		}
	}

	var closest *models.CWATime
	minDiff := math.MaxFloat64
	for i := range times {
		t := parseTimeSafe(times[i].StartTime)
		if t.IsZero() {
			t = parseTimeSafe(times[i].DataTime)
		}
		if t.IsZero() {
			continue
		}
		if diff := math.Abs(float64(t.Sub(now))); diff < minDiff {
			minDiff = diff
			closest = &times[i]
		}
	}
	return closest
}

func parseTimeSafe(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// CWA format: "2024-01-15T12:00:00+08:00"
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05-07:00", s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func extractValue(abbr string, evs []models.CWAElementValue) string {
	if len(evs) == 0 {
		return ""
	}
	ev := evs[0]

	switch abbr {
	case "T":
		return ev.Temperature
	case "Td":
		return ev.DewPoint
	case "RH":
		return ev.RelativeHumidity
	case "AT":
		return ev.ApparentTemperature
	case "UVI":
		return ev.UVIndex
	case "WS":
		return ev.WindSpeed
	case "WD":
		return ev.WindDirection
	case "PoP12h", "PoP6h":
		return ev.ProbabilityOfPrecipitation
	case "Wx":
		return ev.Weather
	case "WeatherDescription":
		return ev.WeatherDescription
	default:
		return ""
	}
}
