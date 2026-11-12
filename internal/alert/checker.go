package alert

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// maxAlertAge 地震、海嘯超過這個時間就不推（資料源只給最新一筆時，舊地震才不會每天重推）.
const maxAlertAge = 3 * time.Hour

// tsunamiTriggerPrefix 新地震順便查海嘯的去重鍵前綴（後接地震編號；鍵不含使用者 ID）.
const tsunamiTriggerPrefix = "cwa:tsunami-trigger:"

// tsunamiTriggerWarnAbove 本地去重紀錄筆數超過這個數字只發 Warn（每個編號 3 小時內最多一筆，實務遠低於此）.
const tsunamiTriggerWarnAbove = 1_000

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

// ErasureChecker [F241-12][F242-02] 查使用者在 since 之後是否刪除過資料（刪除標記由刪除流程擁有，這裡只借用）.
type ErasureChecker interface {
	ErasedSince(ctx context.Context, userID string, since time.Time) bool
}

// Checker 警報檢查與推播器.
type Checker struct {
	alertSubRepo Subscribers
	notifier     Notifier
	sent         *sentLog
	erasure      ErasureChecker // nil = 視為從未刪除
	episodes     *episodes
	cfg          *config.Config
	now          func() time.Time
	rdb          *redis.Client // 海嘯觸發去重用；nil 時只用本地記憶體

	cwaOnce   sync.Once
	cwaClient *cwa.Client // [F30-40] nil 時第一次用到才建預設客戶端
	// [F30-42] trigger 新地震觸發海嘯的本地去重（Redis 為 nil 或出錯時的備援）.
	trigger *localcache.Cache[struct{}]

	coldMu   sync.Mutex
	coldNext time.Time // 下次可以抓低溫明細的時間；抓失敗會清掉，下一輪重抓
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
		episodes:     newEpisodes(redisClient),
		cfg:          cfg,
		now:          time.Now,
		rdb:          redisClient,
		trigger:      localcache.NewNoEvict[struct{}]("alert-tsunami-trigger", maxAlertAge, tsunamiTriggerWarnAbove),
	}
}

// WithCWAClient [F30-40] 注入氣象署客戶端（app 傳入與預報端共用同一份記帳的客戶端）.
func (c *Checker) WithCWAClient(cl *cwa.Client) *Checker {
	c.cwaClient = cl
	return c
}

// client 回氣象署客戶端；沒注入時第一次用到才建預設的（記憶體記帳，讓只用 NewChecker 的測試不必改建構碼）.
func (c *Checker) client() *cwa.Client {
	c.cwaOnce.Do(func() {
		if c.cwaClient != nil {
			return
		}
		settings := c.cfg.CWASettings()
		c.cwaClient = cwa.NewClient(cwa.ClientOptions{
			BaseURL:        c.cfg.Weather.CWABaseURL,
			AuthKey:        c.cfg.Weather.CWAAuthKey,
			AttemptTimeout: cwa.AlertAttemptTimeout,
		}, cwa.NewLedger(nil, &settings, nil, time.Now), &settings)
	})
	return c.cwaClient
}

// WithErasureCheck [F241-12] 設定刪除標記查詢；未設定時推播不查刪除標記.
func (c *Checker) WithErasureCheck(e ErasureChecker) *Checker {
	c.erasure = e
	return c
}

// ForgetRecipient [F241-11] 刪除這位使用者的所有推播紀錄（Redis 與本地）.
func (c *Checker) ForgetRecipient(ctx context.Context, userID string) error {
	return c.sent.forgetRecipient(ctx, userID)
}

// erasedSince [F241-12][F242-02] 未設定查詢時視為從未刪除.
func (c *Checker) erasedSince(ctx context.Context, userID string, since time.Time) bool {
	return c.erasure != nil && c.erasure.ErasedSince(ctx, userID, since)
}

// CheckAndNotify 依序執行三個檢查（特報、地震、海嘯）並合併錯誤；三個排程各自呼叫下面三個方法.
func (c *Checker) CheckAndNotify(ctx context.Context) error {
	return errors.Join(c.CheckWarnings(ctx), c.CheckEarthquakes(ctx), c.CheckTsunami(ctx))
}

// CheckWarnings [F30-41] 特報排程：低溫 → 001 → 鄉鎮明細.
// [F24-99] 沒有人開啟天氣特報就不呼叫氣象署；查訂閱失敗也不呼叫.
func (c *Checker) CheckWarnings(ctx context.Context) error {
	if err := c.checkWeatherWarnings(ctx); err != nil {
		return fmt.Errorf("weather warnings: %w", err)
	}
	return nil
}

// CheckEarthquakes [F30-42] 地震排程：每輪抓最新幾筆、由舊到新處理；抓到新地震順便查一次海嘯.
// [F24-99] 沒有人開啟地震海嘯通知就不呼叫氣象署；查訂閱失敗也不呼叫.
func (c *Checker) CheckEarthquakes(ctx context.Context) error {
	subs, err := c.quakeSubscribers(ctx)
	if err != nil || len(subs) == 0 {
		return err
	}
	triggerTsunami, err := c.checkEarthquakes(ctx, subs)
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("earthquakes: %w", err))
	}
	if triggerTsunami {
		if err := c.checkTsunamiWarnings(ctx, subs); err != nil {
			errs = append(errs, fmt.Errorf("tsunami warnings: %w", err))
		}
	}
	return errors.Join(errs...)
}

// CheckTsunami [F30-41] 海嘯排程.
func (c *Checker) CheckTsunami(ctx context.Context) error {
	subs, err := c.quakeSubscribers(ctx)
	if err != nil || len(subs) == 0 {
		return err
	}
	if err := c.checkTsunamiWarnings(ctx, subs); err != nil {
		return fmt.Errorf("tsunami warnings: %w", err)
	}
	return nil
}

func (c *Checker) quakeSubscribers(ctx context.Context) ([]alertsub.AlertSubscription, error) {
	subs, err := c.alertSubRepo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
	if err != nil {
		return nil, fmt.Errorf("earthquake and tsunami subscribers: %w", err)
	}
	return subs, nil
}

// checkWeatherWarnings 依 W-C0033-001 逐縣市通知；強風、豪大雨再用鄉鎮明細縮小範圍；低溫不在 001，另外直接抓 004.
func (c *Checker) checkWeatherWarnings(ctx context.Context) error {
	has, err := c.alertSubRepo.HasEnabledByType(ctx, alertsub.TypeWeatherWarning)
	if err != nil {
		return fmt.Errorf("find subscribers: %w", err)
	}
	if !has {
		return nil
	}
	// [F24-99] 放在 001 之前：001 抓失敗也不影響低溫.
	c.checkColdWarnings(ctx)
	resp, err := FetchWeatherWarnings(ctx, c.client(), c.cfg.Alert.WeatherDataset)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	details := &capDetails{client: c.client(), now: c.now(), got: map[string]map[string][]capNotice{}}
	for _, loc := range resp.Records.Location {
		// CWA 回傳「臺」，訂閱存的是「台」，統一轉換
		county := strings.ReplaceAll(loc.LocationName, "臺", "台")
		var general []models.CWAHazard
		for _, h := range loc.HazardCondition.Hazards {
			switch {
			case h.Info.Phenomena == "":
			case h.Info.Phenomena == phenomenonCold:
				// [F24-99] 低溫由 checkColdWarnings 看 004 通知；001 若也列了，不再另推一則縣市層級的.
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
		slog.Error("find weather warning subs", "county", county, "error", redact.Error(err))
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
		// [F24-99] 每個現象各自記進行中標記：其中一個解除時，留下的那個 id 跟之前一樣，不重推.
		levels = append(levels, c.weatherID(ctx, county+":"+h.Info.Phenomena+h.Info.Significance))
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
			// [F241-06] 以「<現象>特報」開頭精確比對（豪雨不吃進大豪雨）；headline 格式依 testdata
			// 004/006 為「<現象>特報」開頭，生效中雨量明細的格式未驗證，格式不符會走退回縣市通知（多發不漏發）.
			if dataset == datasetRain && !strings.HasPrefix(n.headline, hazard.Phenomena+"特報") {
				continue
			}
			notices = append(notices, n)
		}
	}
	if len(notices) == 0 {
		msg := fmt.Sprintf("[天氣特報] %s\n地區：%s\n請注意防範", hazard.Phenomena, county)
		c.deliver(ctx, "weather", c.weatherID(ctx, county+":"+level), msg, subs)
		return
	}
	c.notifyNotices(ctx, county, hazard.Phenomena, level, notices, subs, episodeTTL)
}

// notifyNotices 每則有效明細只通知它列出的鄉鎮與訂閱整縣的人；去重 id 含明細等級，升級（黃 → 橙）會再通知.
func (c *Checker) notifyNotices(
	ctx context.Context, county, phenomenon, level string, notices []capNotice, subs []alertsub.AlertSubscription, ttl time.Duration,
) {
	for _, n := range notices {
		id := c.weatherIDTTL(ctx, county+":"+level+":"+n.level, ttl)
		if n.countyWide {
			msg := fmt.Sprintf("[天氣特報] %s(%s)\n地區：%s\n請注意防範", phenomenon, n.level, county)
			c.deliver(ctx, "weather", id, msg, subs)
			continue
		}
		msg := fmt.Sprintf("[天氣特報] %s(%s)\n地區：%s%s\n請注意防範", phenomenon, n.level, county, strings.Join(n.towns, "、"))
		c.deliver(ctx, "weather", id, msg, townSubs(subs, county, n.towns))
	}
}

// weatherID 天氣特報的去重 id：key 加上這一波的開始時間.
// [F24-99] 特報持續期間 id 不變（每人只推一次）；解除超過 episodeTTL 後再發布換新 id、再推一次.
func (c *Checker) weatherID(ctx context.Context, key string) string {
	return c.weatherIDTTL(ctx, key, episodeTTL)
}

func (c *Checker) weatherIDTTL(ctx context.Context, key string, ttl time.Duration) string {
	return key + "@" + c.episodes.startTTL(ctx, key, c.now(), ttl)
}

// coldEpisodeTTL [F30-43] 低溫特報進行中標記的存活時間 = max(2 小時, 2 × 低溫實際最大間隔)；
// 實際最大間隔 = max(ColdInterval 設定, 特報 cron 未來 24 小時內相鄰兩次觸發的最大間隔)（低溫掛在特報排程上，兩者取大）.
// 標記存活得比兩次抓取之間短，同一則低溫特報會被當成新一波、對同一批人重推. 特報排程 off 時低溫不抓，維持 2 小時.
func (c *Checker) coldEpisodeTTL() time.Duration {
	s := c.cfg.CWASettings()
	if s.WarningCron == config.CronOff {
		return episodeTTL
	}
	gap := max(s.ColdInterval, maxCronGap(s.WarningCron, c.now()))
	return max(episodeTTL, 2*gap)
}

// maxCronGap spec（Asia/Taipei）從 now 起 24 小時內相鄰兩次觸發的最大間隔；解析失敗或不足兩次觸發回 0.
func maxCronGap(spec string, now time.Time) time.Duration {
	sched, err := config.ParseCWACron(spec)
	if err != nil {
		return 0
	}
	horizon := now.Add(24 * time.Hour)
	var largest time.Duration
	prev := time.Time{}
	for t := sched.Next(now); !t.IsZero() && !t.After(horizon); t = sched.Next(t) {
		if !prev.IsZero() {
			largest = max(largest, t.Sub(prev))
		}
		prev = t
	}
	return largest
}

// checkColdWarnings 低溫特報不在總表 W-C0033-001（氣象署產品說明 6-3-1 只列颱風、豪(大)雨、陸上強風、濃霧），
// 直接抓低溫明細 W-C0033-004，照明細列的鄉鎮通知.
// [F24-99][F30-43] 每個設定的低溫間隔（CWASettings().ColdInterval）最多抓一次；抓失敗不等，下一輪重抓（有抓到就要通知）.
func (c *Checker) checkColdWarnings(ctx context.Context) {
	now := c.now()
	if !c.claimColdFetch(now) {
		return
	}
	resp, err := FetchCapWarnings(ctx, c.client(), datasetCold)
	if err != nil {
		c.releaseColdFetch()
		slog.Warn("fetch cold warnings, retrying next check", failAttrs(err)...)
		return
	}
	byCounty := capAreas(resp, now)
	for _, county := range region.Counties {
		notices := byCounty[county]
		if len(notices) == 0 {
			continue
		}
		if subs := c.countySubs(ctx, county); len(subs) > 0 {
			c.notifyNotices(ctx, county, phenomenonCold, phenomenonCold+"特報", notices, subs, c.coldEpisodeTTL())
		}
	}
}

// claimColdFetch 到了抓低溫明細的時間就先佔下這一次，並行的檢查不會重複抓.
func (c *Checker) claimColdFetch(now time.Time) bool {
	c.coldMu.Lock()
	defer c.coldMu.Unlock()
	if now.Before(c.coldNext) {
		return false
	}
	c.coldNext = now.Add(c.cfg.CWASettings().ColdInterval)
	return true
}

// releaseColdFetch 抓失敗：放掉佔位，下一輪重抓.
func (c *Checker) releaseColdFetch() {
	c.coldMu.Lock()
	defer c.coldMu.Unlock()
	c.coldNext = time.Time{}
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
	client *cwa.Client
	now    time.Time
	got    map[string]map[string][]capNotice // 資料集 → 縣市 → 有效明細；nil 表示抓取失敗
}

func (d *capDetails) get(ctx context.Context, dataset string) (map[string][]capNotice, bool) {
	if m, done := d.got[dataset]; done {
		return m, m != nil
	}
	resp, err := FetchCapWarnings(ctx, d.client, dataset)
	if err != nil {
		// [F30-43] 被額度擋（errors.Is ErrQuotaReserved）也走這裡：退回縣市通知，不是不推.
		slog.Warn("fetch township warnings, notifying by county", append([]any{"dataset", dataset}, failAttrs(err)...)...)
		d.got[dataset] = nil
		return nil, false
	}
	m := capAreas(resp, d.now)
	d.got[dataset] = m
	return m, true
}

// checkEarthquakes [F30-42] 回傳「這輪有新地震該順便查海嘯」.
func (c *Checker) checkEarthquakes(ctx context.Context, subs []alertsub.AlertSubscription) (bool, error) {
	limit := c.cfg.CWASettings().EarthquakeLimit
	resp, err := FetchEarthquakes(ctx, c.client(), c.cfg.Alert.EarthquakeDataset, limit)
	if err != nil {
		return false, fmt.Errorf("fetch: %w", err)
	}

	triggerTsunami := false
	for _, eq := range newestQuakes(resp.Records.Earthquake, limit) {
		if c.tooOld("earthquake", eq.EarthquakeInfo.OriginTime) {
			continue
		}

		msg := fmt.Sprintf(
			"[地震報告] %s\n規模 M%.1f，深度 %.0f 公里\n震央：%s\n%s",
			eq.EarthquakeInfo.OriginTime,
			eq.EarthquakeInfo.Magnitude.Value,
			eq.EarthquakeInfo.FocalDepth,
			eq.EarthquakeInfo.EpiCenter.Location,
			eq.ReportContent,
		)

		// 這則地震這輪有新佔到收件人、且是第一次為它查海嘯，才觸發（一輪最多一次由呼叫端保證）.
		if c.deliver(ctx, "earthquake", strconv.Itoa(eq.EarthquakeNo), msg, subs) > 0 && c.claimTsunamiTrigger(ctx, eq.EarthquakeNo) {
			triggerTsunami = true
		}
	}

	return triggerTsunami, nil
}

// claimTsunamiTrigger [F30-42] 每個地震編號最多觸發一次海嘯檢查：Redis SET NX（3 小時）；Redis 為 nil 或出錯退本地記憶體.
func (c *Checker) claimTsunamiTrigger(ctx context.Context, earthquakeNo int) bool {
	key := tsunamiTriggerPrefix + strconv.Itoa(earthquakeNo)
	if c.rdb != nil {
		ok, err := c.rdb.SetNX(ctx, key, "1", maxAlertAge).Result()
		if err == nil {
			return ok
		}
		slog.Warn("tsunami trigger store unavailable, claim recorded in memory", "error", redact.Error(err))
	}
	return c.trigger.SetIfAbsent(key, struct{}{}, maxAlertAge)
}

// quakeEntry 排序用：地震與解析後的發生時間.
type quakeEntry struct {
	eq models.CWAEarthquake
	t  time.Time
	ok bool
}

// newestQuakes [F30-42] 依發生時間由新到舊取前 limit 筆，再由舊到新回傳（補漏時先推較舊的）.
// 時間解析失敗的排在最後（取筆數時最後被取、處理時最後處理），彼此保持回應順序. 不假設 API 的預設排序（未驗證）.
func newestQuakes(list []models.CWAEarthquake, limit int) []models.CWAEarthquake {
	entries := make([]quakeEntry, len(list))
	for i := range list {
		t, ok := parseCWATime(list[i].EarthquakeInfo.OriginTime)
		entries[i] = quakeEntry{eq: list[i], t: t, ok: ok}
	}
	sortQuakeEntries(entries, true)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	sortQuakeEntries(entries, false)
	out := make([]models.CWAEarthquake, len(entries))
	for i := range entries {
		out[i] = entries[i].eq
	}
	return out
}

// sortQuakeEntries 穩定排序：解析成功的排前面；同為成功時 newestFirst 由新到舊、否則由舊到新. 解析失敗的保持原順序排在最後.
func sortQuakeEntries(entries []quakeEntry, newestFirst bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ok != entries[j].ok {
			return entries[i].ok
		}
		if newestFirst {
			return entries[i].t.After(entries[j].t)
		}
		return entries[i].t.Before(entries[j].t)
	})
}

func (c *Checker) checkTsunamiWarnings(ctx context.Context, subs []alertsub.AlertSubscription) error {
	resp, err := FetchTsunamiWarnings(ctx, c.client(), c.cfg.Alert.TsunamiDataset)
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

		msg := fmt.Sprintf(
			"[海嘯警報] %s\n%s\n震央：%s，規模 M%.1f\n請即刻遠離海岸，前往高地避難！",
			t.ReportType,
			t.ReportContent,
			t.EarthquakeInfo.EpiCenter.Location,
			t.EarthquakeInfo.Magnitude.Value,
		)

		c.deliver(ctx, "tsunami", fmt.Sprintf("%d_%s", t.TsunamiNo, t.ReportNo), msg, subs)
	}

	return nil
}

// failAttrs [F30-44] 失敗 log 的欄位：氣象署錯誤只記分類（位址、主機名、內層原文都不進 log）；
// 其他錯誤（例如查訂閱失敗）照舊記遮蔽後的訊息.
//
// [F302-02] errors.Join（含巢狀）先攤平成葉子：可分類的葉子依出現順序去重、逗號串接成 class；
// 其餘葉子各自遮蔽後以 "; " 串接成 error；兩種都有就兩欄都輸出. 單一錯誤的輸出與攤平前逐字相同.
func failAttrs(err error) []any {
	var classes, others []string
	for _, leaf := range flattenErrors(err) {
		var ce *cwa.Error
		if errors.As(leaf, &ce) || errors.Is(leaf, cwa.ErrQuotaReserved) {
			if class := cwa.ClassOf(leaf); !slices.Contains(classes, class) {
				classes = append(classes, class)
			}
			continue
		}
		others = append(others, redact.Error(leaf))
	}
	var attrs []any
	if len(classes) > 0 {
		attrs = append(attrs, "class", strings.Join(classes, ","))
	}
	if len(others) > 0 {
		attrs = append(attrs, "error", strings.Join(others, "; "))
	}
	return attrs
}

// flattenErrors [F302-09] 遞迴展開實作 Unwrap() []error 的錯誤（errors.Join），回傳非 nil 的葉子；其他錯誤本身就是葉子.
// 展開結果為空的多重錯誤當成一片葉子；*cwa.Error 不往下展開（保住只回 class 的遮蔽）；
// 單層包裝底下有 2 個以上葉子時才展開，只包 1 個錯誤的照舊整片保留（前綴不丟）.
func flattenErrors(err error) []error {
	if err == nil {
		return nil
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range multi.Unwrap() {
			out = append(out, flattenErrors(e)...)
		}
		if len(out) == 0 {
			return []error{err}
		}
		return out
	}
	if _, ok := err.(*cwa.Error); ok {
		return []error{err}
	}
	if inner := errors.Unwrap(err); inner != nil {
		if leaves := flattenErrors(inner); len(leaves) >= 2 {
			return leaves
		}
	}
	return []error{err}
}

// deliver 逐位推播：每個人各自記錄是否送過；失敗的人下一輪重試（有上限），已送過的不會再收到.
// [F30-42] 回傳本次新佔到的收件人數（sent + failed）.
func (c *Checker) deliver(ctx context.Context, kind, id, msg string, subs []alertsub.AlertSubscription) int {
	var sent, failed int
	for i := range subs {
		if ctx.Err() != nil {
			break // 關機中：還沒送的人等重啟後的下一輪
		}
		// [F242-01] 查刪除標記與清紀錄都不受本輪逾時或關機影響（go-redis 不重試 ctx 錯誤）；Push 仍用 ctx，關機時照常中斷.
		bg := context.WithoutCancel(ctx)
		key := kind + ":" + id + ":" + subs[i].UserID
		if !c.sent.claim(ctx, key) {
			continue
		}
		// [F241-12] 「寫後再查」：刪除流程先記刪除標記、後清推播紀錄（webhook 的 handleDeleteDataConfirm 先 erased.mark
		// 再 eraseUserData），所以佔位若寫在清除之後，這裡重查一定看得到標記；剛佔的鍵刪掉、不推播給已刪除資料的人.
		// [F242-02] 以訂閱的更新時間當切點：刪除之後才重新訂閱的人照推.
		if c.erasedSince(bg, subs[i].UserID, subs[i].UpdateTime) {
			c.sent.forgetKey(bg, key)
			continue
		}
		if err := c.notifier.Push(ctx, subs[i].UserID, msg); err != nil {
			failed++
			// 即使 ctx 已經取消（推到一半關機）也要釋放佔位，否則這個人 7 天內都收不到這則.
			c.sent.failed(bg, key)
			// [F241-12] failed 可能在清除之後才寫入重試計數；寫完再查一次，命中就把佔位與計數都刪掉.
			if c.erasedSince(bg, subs[i].UserID, subs[i].UpdateTime) {
				c.sent.forgetKey(bg, key)
			}
			slog.Error("push alert", "kind", kind, "error", redact.Error(err))
			continue
		}
		sent++
	}
	if sent+failed > 0 {
		slog.Info("alert delivered", "kind", kind, "id", id, "sent", sent, "failed", failed)
	}
	return sent + failed
}

// cwaTimeLayouts 氣象署時間格式（台灣時間）.
var cwaTimeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339}

// tooOld 地震、海嘯的發生時間超過 maxAlertAge 就不推. 時間解析不了時照常推（靠逐人紀錄避免重送）.
func (c *Checker) tooOld(kind, originTime string) bool {
	if t, ok := parseCWATime(originTime); ok {
		return c.now().Sub(t) > maxAlertAge
	}
	slog.Warn("unparsable alert time", "kind", kind, "value", originTime)
	return false
}

// parseCWATime 解析氣象署時間字串（沒有時區視為台灣時間）.
func parseCWATime(v string) (time.Time, bool) {
	for _, layout := range cwaTimeLayouts {
		if t, err := time.ParseInLocation(layout, v, taipei); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// taipei 氣象署時間沒有時區時視為台灣時間.
var taipei = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Taipei"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}()
