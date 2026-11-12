package alert

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	emptyCapBody     = `{"success":"true","records":{"info":[]}}`
	emptyTsunamiBody = `{"success":"true","records":{"Tsunami":[]}}`
	emptyQuakeBody   = `{"success":"true","records":{"Earthquake":[]}}`
	// rainWarningBody 001：臺北市豪雨特報（豪雨會再去抓 003 鄉鎮明細）.
	rainWarningBody = `{"success":"true","records":{"location":[{"locationName":"臺北市","hazardConditions":` +
		`{"hazards":[{"info":{"phenomena":"豪雨","significance":"特報"}}]}}]}}`
)

// routeFeed 依資料集代號回固定內容的假氣象署：記請求數與 query；hang 的資料集擋到請求被取消.
type routeFeed struct {
	mu      sync.Mutex
	bodies  map[string]string
	hang    map[string]bool
	hits    map[string]int
	queries map[string]url.Values
	stop    chan struct{}
	srv     *httptest.Server
}

func newRouteFeed(t *testing.T) *routeFeed {
	t.Helper()
	f := &routeFeed{
		bodies: map[string]string{
			"W-C0033-001": noWarnings, "W-C0033-003": emptyCapBody, "W-C0033-004": emptyCapBody, "W-C0033-006": emptyCapBody,
			"E-A0015-001": emptyQuakeBody, "E-A0014-001": emptyTsunamiBody,
		},
		hang: map[string]bool{}, hits: map[string]int{}, queries: map[string]url.Values{}, stop: make(chan struct{}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.stop) }) // 先於 srv.Close 執行：放行卡住的請求
	return f
}

func (f *routeFeed) handle(w http.ResponseWriter, r *http.Request) {
	ds := path.Base(r.URL.Path)
	f.mu.Lock()
	f.hits[ds]++
	f.queries[ds] = r.URL.Query()
	hang, body := f.hang[ds], f.bodies[ds]
	f.mu.Unlock()
	if hang {
		select {
		case <-r.Context().Done():
		case <-f.stop:
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (f *routeFeed) count(ds string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[ds]
}

func (f *routeFeed) query(ds string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[ds]
}

func (f *routeFeed) set(ds, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[ds] = body
}

// newFeedChecker 連到 feed 的檢查器；mod 可改設定（在第一次用到預設客戶端之前）.
func newFeedChecker(t *testing.T, f *routeFeed, subs *fakeSubs, n *fakeNotifier, mod func(*config.Config)) *Checker {
	t.Helper()
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: f.srv.URL, CWAAuthKey: "test-key"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	if mod != nil {
		mod(cfg)
	}
	c := NewChecker(subs, n, nil, cfg)
	c.now = func() time.Time { return checkNow }
	c.sent = newSentLogWithClock(nil, c.now)
	return c
}

// quakeSubs 只有一位地震海嘯訂閱者；weatherAndQuakeSubs 另加一位台北市特報訂閱者.
func quakeSubs() *fakeSubs {
	return &fakeSubs{subs: []alertsub.AlertSubscription{{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true}}}
}

func weatherAndQuakeSubs() *fakeSubs {
	s := quakeSubs()
	s.subs = append(s.subs, alertsub.AlertSubscription{UserID: "U1", AlertType: alertsub.TypeWeatherWarning, Regions: []string{"台北市"}, Enabled: true})
	return s
}

// quakeSpec 一筆地震（編號與發生時間字串）.
type quakeSpec struct {
	no     int
	origin string
}

func quakeBody(specs ...quakeSpec) string {
	items := make([]string, 0, len(specs))
	for _, q := range specs {
		items = append(items, fmt.Sprintf(`{"EarthquakeNo":%d,"ReportContent":"花蓮縣近海地震","EarthquakeInfo":{"OriginTime":%q,`+
			`"FocalDepth":10,"Epicenter":{"Location":"花蓮縣政府東方 20 公里"},"EarthquakeMagnitude":{"MagnitudeValue":5.8}}}`, q.no, q.origin))
	}
	return `{"success":"true","records":{"Earthquake":[` + strings.Join(items, ",") + `]}}`
}

// logBuf 可並行寫入的 log 緩衝.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureText 把預設 slog 換成文字輸出（Debug 以上）；測試結束還原.
func captureText(t *testing.T) *logBuf {
	t.Helper()
	b := &logBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

// deniedClient 額度吃緊的客戶端：過去 1 小時已記 900 筆、依預設排程 24 小時內還要打約 760 次 → 超過 cap 950，
// 非免擋呼叫（含重試）被擋，免擋的第一次嘗試照打. 900 筆用免擋地震呼叫打到另一台假伺服器記上去.
func deniedClient(t *testing.T, baseURL string) *cwa.Client {
	t.Helper()
	loc, err := cwa.LoadTaipei(nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := (&config.Config{}).CWASettings()
	ledger := cwa.NewLedger(nil, settings, loc, time.Now)
	seedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(seedSrv.Close)
	seeder := cwa.NewClient(cwa.ClientOptions{BaseURL: seedSrv.URL, AuthKey: "seed"}, ledger, settings)
	for range 900 {
		var out struct{}
		spec := cwa.CallSpec{Stream: cwa.StreamEarthquake, Exempt: true, Dataset: "seed"}
		if err := seeder.GetJSON(context.Background(), spec, "seed", nil, &out); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
	return cwa.NewClient(cwa.ClientOptions{BaseURL: baseURL, AuthKey: "test-key", Backoff: time.Millisecond}, ledger, settings)
}
