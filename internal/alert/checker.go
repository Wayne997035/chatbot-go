package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// maxAlertAge 地震、海嘯超過這個時間就不推（資料源只給最新一筆時，舊地震才不會每天重推）.
const maxAlertAge = 3 * time.Hour

// Notifier 推播通知介面，解耦 Checker 與 LINE Push 實作.
type Notifier interface {
	Push(ctx context.Context, userID, text string) error
}

// Checker 警報檢查與推播器.
type Checker struct {
	alertSubRepo alertsub.AlertSubRepository
	notifier     Notifier
	sent         *sentLog
	cfg          *config.Config
	now          func() time.Time
}

// NewChecker 建立警報檢查器.
func NewChecker(
	repo alertsub.AlertSubRepository,
	notifier Notifier,
	redisClient *redis.Client,
	cfg *config.Config,
) *Checker {
	return &Checker{
		alertSubRepo: repo,
		notifier:     notifier,
		sent:         newSentLog(redisClient),
		cfg:          cfg,
		now:          time.Now,
	}
}

// CheckAndNotify 執行一次警報檢查並推播通知.
func (c *Checker) CheckAndNotify(ctx context.Context) error {
	baseURL := c.cfg.Weather.CWABaseURL
	authKey := c.cfg.Weather.CWAAuthKey

	var errs []error

	if err := c.checkWeatherWarnings(ctx, baseURL, authKey); err != nil {
		errs = append(errs, fmt.Errorf("weather warnings: %w", err))
	}
	if err := c.checkEarthquakes(ctx, baseURL, authKey); err != nil {
		errs = append(errs, fmt.Errorf("earthquakes: %w", err))
	}
	if err := c.checkTsunamiWarnings(ctx, baseURL, authKey); err != nil {
		errs = append(errs, fmt.Errorf("tsunami warnings: %w", err))
	}

	return errors.Join(errs...)
}

func (c *Checker) checkWeatherWarnings(ctx context.Context, baseURL, authKey string) error {
	resp, err := FetchWeatherWarnings(ctx, baseURL, authKey, c.cfg.Alert.WeatherDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	for _, loc := range resp.Records.Location {
		if len(loc.HazardCondition.Hazards) == 0 {
			continue
		}

		// CWA 回傳「臺」，訂閱存的是「台」，統一轉換
		region := strings.ReplaceAll(loc.LocationName, "臺", "台")
		subs, err := c.alertSubRepo.FindEnabledByTypeAndRegion(ctx, alertsub.TypeWeatherWarning, region)
		if err != nil {
			slog.Error("find weather warning subs", "region", region, "error", err)
			continue
		}

		if len(subs) == 0 {
			continue
		}

		var phenomena, levels []string
		for _, h := range loc.HazardCondition.Hazards {
			if h.Info.Phenomena != "" {
				phenomena = append(phenomena, h.Info.Phenomena)
				// 等級也放進去重 key：同一現象升級（特報 → 警報）要再通知.
				levels = append(levels, h.Info.Phenomena+h.Info.Significance)
			}
		}
		if len(phenomena) == 0 {
			continue
		}

		msg := fmt.Sprintf("[天氣特報] %s\n地區：%s\n請注意防範", strings.Join(phenomena, "、"), region)
		c.deliver(ctx, "weather", region+":"+strings.Join(levels, ","), msg, subs)
	}

	return nil
}

func (c *Checker) checkEarthquakes(ctx context.Context, baseURL, authKey string) error {
	resp, err := FetchEarthquakes(ctx, baseURL, authKey, c.cfg.Alert.EarthquakeDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	for _, eq := range resp.Records.Earthquake {
		if c.tooOld("earthquake", eq.EarthquakeInfo.OriginTime) {
			continue
		}
		subs, err := c.alertSubRepo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
		if err != nil {
			slog.Error("find earthquake subs", "error", err)
			continue
		}

		msg := fmt.Sprintf("[地震速報] %s\n規模 M%.1f，深度 %.0f 公里\n震央：%s\n%s",
			eq.EarthquakeInfo.OriginTime,
			eq.EarthquakeInfo.Magnitude.Value,
			eq.EarthquakeInfo.FocalDepth,
			eq.EarthquakeInfo.EpiCenter.Location,
			eq.ReportContent,
		)

		c.deliver(ctx, "earthquake", strconv.Itoa(eq.EarthquakeNo), msg, subs)
	}

	return nil
}

func (c *Checker) checkTsunamiWarnings(ctx context.Context, baseURL, authKey string) error {
	resp, err := FetchTsunamiWarnings(ctx, baseURL, authKey, c.cfg.Alert.TsunamiDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	for i := range resp.Records.Tsunami {
		t := &resp.Records.Tsunami[i]
		// 綠色 = 解除，不推播
		if t.ReportColor == "綠色" {
			continue
		}

		if c.tooOld("tsunami", t.EarthquakeInfo.OriginTime) {
			continue
		}
		subs, err := c.alertSubRepo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
		if err != nil {
			slog.Error("find tsunami subs", "error", err)
			continue
		}

		msg := fmt.Sprintf("[海嘯警報] %s\n%s\n震央：%s，規模 M%.1f\n請即刻遠離海岸，前往高地避難！",
			t.ReportType,
			t.ReportContent,
			t.EarthquakeInfo.EpiCenter.Location,
			t.EarthquakeInfo.Magnitude.Value,
		)

		c.deliver(ctx, "tsunami", fmt.Sprintf("%d_%s", t.TsunamiNo, t.ReportNo), msg, subs)
	}

	return nil
}

// deliver 逐位推播：每個人各自記錄是否送過；失敗的人下一輪重試（有上限），已送過的不會再收到.
func (c *Checker) deliver(ctx context.Context, kind, id, msg string, subs []alertsub.AlertSubscription) {
	var sent, failed int
	for i := range subs {
		if ctx.Err() != nil {
			break // 關機中：還沒送的人等重啟後的下一輪
		}
		key := kind + ":" + id + ":" + subs[i].UserID
		if !c.sent.claim(ctx, key) {
			continue
		}
		if err := c.notifier.Push(ctx, subs[i].UserID, msg); err != nil {
			failed++
			// 即使 ctx 已經取消（推到一半關機）也要釋放佔位，否則這個人 7 天內都收不到這則.
			c.sent.failed(context.WithoutCancel(ctx), key)
			slog.Error("push alert", "kind", kind, "error", err)
			continue
		}
		sent++
	}
	if sent+failed > 0 {
		slog.Info("alert delivered", "kind", kind, "id", id, "sent", sent, "failed", failed)
	}
}

// cwaTimeLayouts 氣象署時間格式（台灣時間）.
var cwaTimeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339}

// tooOld 地震、海嘯的發生時間超過 maxAlertAge 就不推. 時間解析不了時照常推（靠逐人紀錄避免重送）.
func (c *Checker) tooOld(kind, originTime string) bool {
	for _, layout := range cwaTimeLayouts {
		if t, err := time.ParseInLocation(layout, originTime, taipei); err == nil {
			return c.now().Sub(t) > maxAlertAge
		}
	}
	slog.Warn("unparsable alert time", "kind", kind, "value", originTime)
	return false
}

// taipei 氣象署時間沒有時區時視為台灣時間.
var taipei = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Taipei"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}()
