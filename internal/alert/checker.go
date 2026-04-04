package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	dedupTTL    = 24 * time.Hour
	dedupKeyFmt = "alert:notified:%s:%s"
)

// Notifier 推播通知介面，解耦 Checker 與 LINE Push 實作.
type Notifier interface {
	Push(ctx context.Context, userID, text string) error
}

// Checker 警報檢查與推播器.
type Checker struct {
	alertSubRepo alertsub.AlertSubRepository
	notifier     Notifier
	redisClient  *redis.Client
	cfg          *config.Config
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
		redisClient:  redisClient,
		cfg:          cfg,
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

		var phenomena []string
		for _, h := range loc.HazardCondition.Hazards {
			if h.Info.Phenomena != "" {
				phenomena = append(phenomena, h.Info.Phenomena)
			}
		}
		if len(phenomena) == 0 {
			continue
		}

		msg := fmt.Sprintf("[天氣特報] %s\n地區：%s\n請注意防範", strings.Join(phenomena, "、"), region)
		dedupKey := fmt.Sprintf(dedupKeyFmt, "weather", region+":"+strings.Join(phenomena, ","))

		if !c.shouldNotify(ctx, dedupKey) {
			continue
		}

		for i := range subs {
			if err := c.notifier.Push(ctx, subs[i].UserID, msg); err != nil {
				slog.Error("push weather warning", "userID", subs[i].UserID, "error", err)
			}
		}

		c.markNotified(ctx, dedupKey)
	}

	return nil
}

func (c *Checker) checkEarthquakes(ctx context.Context, baseURL, authKey string) error {
	resp, err := FetchEarthquakes(ctx, baseURL, authKey, c.cfg.Alert.EarthquakeDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	for _, eq := range resp.Records.Earthquake {
		dedupKey := fmt.Sprintf(dedupKeyFmt, "earthquake", fmt.Sprintf("%d", eq.EarthquakeNo))
		if !c.shouldNotify(ctx, dedupKey) {
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

		for i := range subs {
			if err := c.notifier.Push(ctx, subs[i].UserID, msg); err != nil {
				slog.Error("push earthquake", "userID", subs[i].UserID, "error", err)
			}
		}

		c.markNotified(ctx, dedupKey)
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

		dedupKey := fmt.Sprintf(dedupKeyFmt, "tsunami", fmt.Sprintf("%d_%s", t.TsunamiNo, t.ReportNo))
		if !c.shouldNotify(ctx, dedupKey) {
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

		for j := range subs {
			if err := c.notifier.Push(ctx, subs[j].UserID, msg); err != nil {
				slog.Error("push tsunami", "userID", subs[j].UserID, "error", err)
			}
		}

		c.markNotified(ctx, dedupKey)
	}

	return nil
}

// shouldNotify 檢查 Redis dedup key 是否已存在；Redis 不可用時預設允許通知.
func (c *Checker) shouldNotify(ctx context.Context, key string) bool {
	if c.redisClient == nil {
		return true
	}
	exists, err := c.redisClient.Exists(ctx, key).Result()
	if err != nil {
		slog.Warn("check dedup key", "key", key, "error", err)
		return true
	}
	return exists == 0
}

// markNotified 設定 Redis dedup key.
func (c *Checker) markNotified(ctx context.Context, key string) {
	if c.redisClient == nil {
		return
	}
	if err := c.redisClient.Set(ctx, key, "1", dedupTTL).Err(); err != nil {
		slog.Warn("mark notified", "key", key, "error", err)
	}
}
