package alert

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/cwa"
	"context"
	"net/url"
	"strconv"
)

// [F30-40] 四個抓取都經 (*cwa.Client).GetJSON：重試、每次嘗試期限、回應上限、記帳都在共用層.

// FetchWeatherWarnings 從 CWA 取得天氣特報 (W-C0033-001)；總表免擋額度判斷（規格 T21、T26）.
func FetchWeatherWarnings(ctx context.Context, c *cwa.Client, dataset string) (*models.CWAAlertResponse, error) {
	spec := cwa.CallSpec{Stream: cwa.StreamWarning, Exempt: true, Dataset: dataset}
	return fetchJSON[models.CWAAlertResponse](ctx, c, spec, dataset, jsonQuery())
}

// FetchCapWarnings 從 CWA 取得有鄉鎮明細的天氣特報（W-C0033-003/004/006）；一律受額度判斷.
// 低溫 004 記在 cold stream，鄉鎮明細 003 / 006 記在 warning stream.
func FetchCapWarnings(ctx context.Context, c *cwa.Client, dataset string) (*models.CWACapResponse, error) {
	stream := cwa.StreamWarning
	if dataset == datasetCold {
		stream = cwa.StreamCold
	}
	spec := cwa.CallSpec{Stream: stream, Exempt: false, Dataset: dataset}
	return fetchJSON[models.CWACapResponse](ctx, c, spec, dataset, jsonQuery())
}

// FetchEarthquakes 從 CWA 取得最新 limit 筆地震報告 (E-A0015-001)；免擋.
func FetchEarthquakes(ctx context.Context, c *cwa.Client, dataset string, limit int) (*models.CWAEarthquakeResponse, error) {
	q := jsonQuery()
	q.Set("limit", strconv.Itoa(limit))
	spec := cwa.CallSpec{Stream: cwa.StreamEarthquake, Exempt: true, Dataset: dataset}
	return fetchJSON[models.CWAEarthquakeResponse](ctx, c, spec, dataset, q)
}

// FetchTsunamiWarnings 從 CWA 取得海嘯警報 (E-A0014-001)；海嘯排程與地震觸發的都免擋.
func FetchTsunamiWarnings(ctx context.Context, c *cwa.Client, dataset string) (*models.CWATsunamiResponse, error) {
	spec := cwa.CallSpec{Stream: cwa.StreamTsunami, Exempt: true, Dataset: dataset}
	return fetchJSON[models.CWATsunamiResponse](ctx, c, spec, dataset, jsonQuery())
}

func jsonQuery() url.Values {
	return url.Values{"format": {"JSON"}}
}

// fetchJSON 通用抓取：每次抓取各自從傳入的 ctx 衍生一份 FetchBudget 的期限，一份用完不影響下一份（規格 T3）.
// 授權碼由共用層放 header，不進 URL 也不進錯誤字串.
func fetchJSON[T any](ctx context.Context, c *cwa.Client, spec cwa.CallSpec, dataset string, q url.Values) (*T, error) {
	ctx, cancel := context.WithTimeout(ctx, c.FetchBudget())
	defer cancel()
	var out T
	if err := c.GetJSON(ctx, spec, dataset, q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
