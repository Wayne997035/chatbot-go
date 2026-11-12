package weather

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
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

// 預報同步的套件層接縫（測試可換）.
var (
	// forecastNow 時鐘；createTime 與時段選擇都用它.
	forecastNow = time.Now
	// forecastLoadTaipei 台灣時區；載入失敗時同步不發任何請求 [F30-52].
	forecastLoadTaipei = func() (*time.Location, error) { return cwa.LoadTaipei(nil) }
	// forecastSleep 每份實際發出請求後的 rate limit 等待；ctx 取消回 ctx.Err() [F30-50].
	forecastSleep = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

const createTimeLayout = "2006-01-02 15:04:05"

// SyncResult 一次預報同步的結果計數.
type SyncResult struct {
	Fetched, Skipped, Failed int
}

// forecastDatasets 預報同步要抓的資料集：F-D0047-003, 007, …, 087（4k+3，22 縣市未來 1 週）加 089（台灣）.
// 不抓 4k+1 的 3 天資料：同一縣市會被 1 週資料整份覆蓋，bot 也只認 1 週的欄位名 [F30-50].
func forecastDatasets() []string {
	ids := make([]string, 0, cwa.ForecastDatasetCount)
	for k := 0; k <= 21; k++ {
		ids = append(ids, fmt.Sprintf("F-D0047-%03d", 4*k+3))
	}
	return append(ids, "F-D0047-089")
}

// errAttr 氣象署錯誤只記分類（不含主機名、IP、解析器位址）；其他錯誤照舊經 redact [F30-54].
func errAttr(err error) slog.Attr {
	var ce *cwa.Error
	if errors.As(err, &ce) || errors.Is(err, cwa.ErrQuotaReserved) {
		return slog.String("class", cwa.ClassOf(err))
	}
	return slog.String("error", redact.Error(err))
}

// FetchAndStore 從 CWA Open Data API 取得天氣資料並存入 MongoDB [F30-50].
// 被額度判斷擋下的資料集算略過（不算失敗、不睡）；error 只在全部失敗時回.
func FetchAndStore(ctx context.Context, client *cwa.Client, rateLimitMs int, repo weatherstore.WeatherRepository) (SyncResult, error) {
	var res SyncResult
	taipei, err := forecastLoadTaipei()
	if err != nil || taipei == nil {
		// 不退回 UTC 或 FixedZone：寧可不抓，也不寫錯時區的 createTime [F30-52].
		return res, errTaipeiUnavailable
	}

	datasets := forecastDatasets()
	var lastErr error
	for _, datasetID := range datasets {
		spec := cwa.CallSpec{Stream: cwa.StreamForecast, Exempt: false, Dataset: datasetID}
		err := fetchAndProcess(ctx, client, spec, taipei, repo)
		switch {
		case err == nil:
			res.Fetched++
		case isQuotaSkip(err):
			res.Skipped++
			continue // 沒發請求，不睡
		default:
			res.Failed++
			lastErr = err
			slog.Warn("fetch weather data", "dataset", datasetID, errAttr(err)) // 繼續處理其他區域，不中斷
		}

		// Rate limiting（只在實際發出請求後）
		if err := forecastSleep(ctx, time.Duration(rateLimitMs)*time.Millisecond); err != nil {
			return res, err
		}
	}

	total := len(datasets)
	// 以前全部失敗（例如授權碼錯誤回 401）也回傳成功，log 會寫「同步完成」.
	if res.Fetched == 0 && res.Skipped == 0 && res.Failed > 0 {
		return res, fmt.Errorf("all %d weather datasets failed: %w", total, lastErr)
	}
	if res.Skipped == total {
		slog.Warn("weather sync skipped by quota")
		return res, nil
	}
	slog.Info("weather sync completed", "fetched", res.Fetched, "skipped", res.Skipped, "failed", res.Failed)
	return res, nil
}

var errTaipeiUnavailable = errors.New("taipei timezone unavailable")

// isQuotaSkip 第 1 次嘗試就被額度擋下（裸 ErrQuotaReserved）才算略過.
// 已發出請求、重試時才被擋的是 *cwa.Error，那是真的失敗，不能算略過 [F30-50].
func isQuotaSkip(err error) bool {
	var ce *cwa.Error
	return errors.Is(err, cwa.ErrQuotaReserved) && !errors.As(err, &ce)
}

func fetchAndProcess(ctx context.Context, client *cwa.Client, spec cwa.CallSpec, taipei *time.Location, repo weatherstore.WeatherRepository) error {
	var cwaResp models.CWAResponse
	if err := client.GetJSON(ctx, spec, spec.Dataset, url.Values{"format": {"JSON"}}, &cwaResp); err != nil {
		return err
	}

	now := forecastNow()
	createTime := now.In(taipei).Format(createTimeLayout)

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
				Dataset:    spec.Dataset,
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
