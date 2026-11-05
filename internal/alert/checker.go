package alert

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// Subscribers 查詢要通知的訂閱者（警報檢查只需要這三個查詢）.
type Subscribers interface {
	FindEnabledByType(ctx context.Context, alertType string) ([]alertsub.AlertSubscription, error)
	FindEnabledByTypeAndRegions(ctx context.Context, alertType string, regions []string) ([]alertsub.AlertSubscription, error)
	HasEnabledByType(ctx context.Context, alertType string) (bool, error)
}

// Checker 警報檢查與推播器.
type Checker struct {
	alertSubRepo Subscribers
	notifier     Notifier
	sent         *sentLog
	cfg          *config.Config
	now          func() time.Time
}

// NewChecker 建立警報檢查器.
func NewChecker(
	repo Subscribers,
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
	if err := c.checkQuakesAndTsunami(ctx, baseURL, authKey); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// checkQuakesAndTsunami 地震與海嘯共用一份訂閱者.
// [F24-99] 沒有人開啟地震海嘯通知就不呼叫氣象署；查訂閱失敗也不呼叫.
func (c *Checker) checkQuakesAndTsunami(ctx context.Context, baseURL, authKey string) error {
	subs, err := c.alertSubRepo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
	if err != nil {
		return fmt.Errorf("earthquake and tsunami subscribers: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	var errs []error
	if err := c.checkEarthquakes(ctx, baseURL, authKey, subs); err != nil {
		errs = append(errs, fmt.Errorf("earthquakes: %w", err))
	}
	if err := c.checkTsunamiWarnings(ctx, baseURL, authKey, subs); err != nil {
		errs = append(errs, fmt.Errorf("tsunami warnings: %w", err))
	}
	return errors.Join(errs...)
}

// checkWeatherWarnings 依 W-C0033-001 逐縣市通知；強風、低溫、豪大雨再用鄉鎮明細縮小範圍.
// [F24-99] 沒有人開啟天氣特報就不呼叫氣象署；查訂閱失敗也不呼叫.
func (c *Checker) checkWeatherWarnings(ctx context.Context, baseURL, authKey string) error {
	has, err := c.alertSubRepo.HasEnabledByType(ctx, alertsub.TypeWeatherWarning)
	if err != nil {
		return fmt.Errorf("find subscribers: %w", err)
	}
	if !has {
		return nil
	}
	resp, err := FetchWeatherWarnings(ctx, baseURL, authKey, c.cfg.Alert.WeatherDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	details := &capDetails{baseURL: baseURL, authKey: authKey, now: c.now(), got: map[string]map[string][]capNotice{}}
	for _, loc := range resp.Records.Location {
		// CWA 回傳「臺」，訂閱存的是「台」，統一轉換
		county := strings.ReplaceAll(loc.LocationName, "臺", "台")
		var general []models.CWAHazard
		for _, h := range loc.HazardCondition.Hazards {
			switch {
			case h.Info.Phenomena == "":
			case phenomenonDataset(h.Info.Phenomena) != "":
				c.notifyTownships(ctx, county, h.Info, details)
			default:
				general = append(general, h)
			}
		}
		if len(general) > 0 {
			c.notifyCounty(ctx, county, general)
		}
	}

	return nil
}

// countySubs 縣市層級要通知的人：訂閱整縣，或訂閱該縣任一鄉鎮.
// [F24-99] 回 nil 表示沒人要通知（或查詢失敗，已記錄）.
func (c *Checker) countySubs(ctx context.Context, county string) []alertsub.AlertSubscription {
	subs, err := c.alertSubRepo.FindEnabledByTypeAndRegions(ctx, alertsub.TypeWeatherWarning, region.Areas(county))
	if err != nil {
		slog.Error("find weather warning subs", "county", county, "error", err)
		return nil
	}
	return subs
}

// notifyCounty 沒有鄉鎮明細的特報：整縣與該縣所有鄉鎮的訂閱者都通知，同一縣市的現象合成一則.
func (c *Checker) notifyCounty(ctx context.Context, county string, hazards []models.CWAHazard) {
	subs := c.countySubs(ctx, county)
	if len(subs) == 0 {
		return
	}
	var phenomena, levels []string
	for _, h := range hazards {
		phenomena = append(phenomena, h.Info.Phenomena)
		// 等級也放進去重 key：同一現象升級（特報 → 警報）要再通知.
		levels = append(levels, h.Info.Phenomena+h.Info.Significance)
	}
	msg := fmt.Sprintf("[天氣特報] %s\n地區：%s\n請注意防範", strings.Join(phenomena, "、"), county)
	c.deliver(ctx, "weather", county+":"+strings.Join(levels, ","), msg, subs)
}

// notifyTownships 有鄉鎮明細的特報：每則有效明細只通知它列出的鄉鎮與訂閱整縣的人.
// [F24-99] 縣市沒有訂閱者就不抓明細；明細抓不到、或沒有任何一則有效明細列了本縣 → 退回縣市通知.
func (c *Checker) notifyTownships(ctx context.Context, county string, hazard models.CWAHazardInfo, details *capDetails) {
	subs := c.countySubs(ctx, county)
	if len(subs) == 0 {
		return
	}
	dataset := phenomenonDataset(hazard.Phenomena)
	level := hazard.Phenomena + hazard.Significance
	byCounty, ok := details.get(ctx, dataset)
	var notices []capNotice
	if ok {
		for _, n := range byCounty[county] {
			// 豪大雨一份明細涵蓋四種雨：headline 不是 001 說的那種雨，就當沒列本縣.
			if dataset == datasetRain && !strings.Contains(n.headline, hazard.Phenomena) {
				continue
			}
			notices = append(notices, n)
		}
	}
	if len(notices) == 0 {
		msg := fmt.Sprintf("[天氣特報] %s\n地區：%s\n請注意防範", hazard.Phenomena, county)
		c.deliver(ctx, "weather", county+":"+level, msg, subs)
		return
	}
	for _, n := range notices {
		id := county + ":" + level + ":" + n.level
		if n.countyWide {
			msg := fmt.Sprintf("[天氣特報] %s(%s)\n地區：%s\n請注意防範", hazard.Phenomena, n.level, county)
			c.deliver(ctx, "weather", id, msg, subs)
			continue
		}
		msg := fmt.Sprintf("[天氣特報] %s(%s)\n地區：%s%s\n請注意防範", hazard.Phenomena, n.level, county, strings.Join(n.towns, "、"))
		c.deliver(ctx, "weather", id, msg, townSubs(subs, county, n.towns))
	}
}

// townSubs 訂閱整縣、或訂閱了其中一個列出鄉鎮的人（從已查到的訂閱者裡篩，不再查資料庫）.
func townSubs(subs []alertsub.AlertSubscription, county string, towns []string) []alertsub.AlertSubscription {
	var out []alertsub.AlertSubscription
	for i := range subs {
		for _, r := range subs[i].Regions {
			if r == county || (strings.HasPrefix(r, county) && slices.Contains(towns, strings.TrimPrefix(r, county))) {
				out = append(out, subs[i])
				break
			}
		}
	}
	return out
}

// capDetails 本輪的鄉鎮明細：同一份資料集一輪只抓一次，抓失敗也記住（本輪退回縣市，不重抓）.
type capDetails struct {
	baseURL, authKey string
	now              time.Time
	got              map[string]map[string][]capNotice // 資料集 → 縣市 → 有效明細；nil 表示抓取失敗
}

func (d *capDetails) get(ctx context.Context, dataset string) (map[string][]capNotice, bool) {
	if m, done := d.got[dataset]; done {
		return m, m != nil
	}
	resp, err := FetchCapWarnings(ctx, d.baseURL, d.authKey, dataset)
	if err != nil {
		slog.Warn("fetch township warnings, notifying by county", "dataset", dataset, "error", err)
		d.got[dataset] = nil
		return nil, false
	}
	m := capAreas(resp, d.now)
	d.got[dataset] = m
	return m, true
}

func (c *Checker) checkEarthquakes(ctx context.Context, baseURL, authKey string, subs []alertsub.AlertSubscription) error {
	resp, err := FetchEarthquakes(ctx, baseURL, authKey, c.cfg.Alert.EarthquakeDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	for _, eq := range resp.Records.Earthquake {
		if c.tooOld("earthquake", eq.EarthquakeInfo.OriginTime) {
			continue
		}

		msg := fmt.Sprintf("[地震報告] %s\n規模 M%.1f，深度 %.0f 公里\n震央：%s\n%s",
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

func (c *Checker) checkTsunamiWarnings(ctx context.Context, baseURL, authKey string, subs []alertsub.AlertSubscription) error {
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
