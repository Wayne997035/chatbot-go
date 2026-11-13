package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSubs struct {
	subs []alertsub.AlertSubscription
	err  error // 非 nil 時每個查詢都回這個錯誤
}

func (f *fakeSubs) FindEnabledByType(_ context.Context, alertType string) ([]alertsub.AlertSubscription, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []alertsub.AlertSubscription
	for i := range f.subs {
		if f.subs[i].AlertType == alertType && f.subs[i].Enabled {
			out = append(out, f.subs[i])
		}
	}
	return out, nil
}

// FindEnabledByTypeAndRegions 同 Mongo 的 $in：訂閱地區含任一個就回，每筆訂閱最多一次.
func (f *fakeSubs) FindEnabledByTypeAndRegions(ctx context.Context, alertType string, regions []string) ([]alertsub.AlertSubscription, error) {
	all, err := f.FindEnabledByType(ctx, alertType)
	if err != nil {
		return nil, err
	}
	var out []alertsub.AlertSubscription
	for i := range all {
		if slices.ContainsFunc(all[i].Regions, func(r string) bool { return slices.Contains(regions, r) }) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

func (f *fakeSubs) HasEnabledByType(ctx context.Context, alertType string) (bool, error) {
	all, err := f.FindEnabledByType(ctx, alertType)
	return len(all) > 0, err
}

// fakeNotifier 記錄每次推播；failures 指定某人還要失敗幾次.
type fakeNotifier struct {
	mu       sync.Mutex
	pushes   []string // userID|text
	attempts map[string]int
	failures map[string]int
}

func (n *fakeNotifier) Push(_ context.Context, userID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts[userID]++
	if n.failures[userID] > 0 {
		n.failures[userID]--
		return errors.New("LINE API returned 500")
	}
	n.pushes = append(n.pushes, userID+"|"+text)
	return nil
}

func (n *fakeNotifier) count(userID, contains string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, p := range n.pushes {
		if strings.HasPrefix(p, userID+"|") && strings.Contains(p, contains) {
			c++
		}
	}
	return c
}

// cwaFeed 假的氣象署 API；內容可以在測試中途修改.
type cwaFeed struct {
	mu         sync.Mutex
	warning    string // Significance，例如 "特報"；空字串表示沒有特報
	quakeTime  string
	tsunami    bool
	tsunamiAge string
}

func (f *cwaFeed) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/W-C0033-001"):
		hazards := "[]"
		if f.warning != "" {
			hazards = `[{"info":{"phenomena":"豪雨","significance":"` + f.warning + `"}}]`
		}
		_, _ = fmt.Fprintf(w, `{"success":"true","records":{"location":[{"locationName":"臺北市","hazardConditions":{"hazards":%s}}]}}`, hazards)
	case strings.HasSuffix(r.URL.Path, "/W-C0033-004"): // 低溫明細：沒有低溫特報
		_, _ = w.Write([]byte(`{"success":"true","records":{"info":[]}}`))
	case strings.HasSuffix(r.URL.Path, "/E-A0015-001"):
		_, _ = fmt.Fprintf(w, `{"success":"true","records":{"Earthquake":[{"EarthquakeNo":115001,"ReportContent":"花蓮縣近海地震",`+
			`"EarthquakeInfo":{"OriginTime":%q,"FocalDepth":10,"Epicenter":{"Location":"花蓮縣政府東方 20 公里"},`+
			`"EarthquakeMagnitude":{"MagnitudeValue":5.8}}}]}}`, f.quakeTime)
	case strings.HasSuffix(r.URL.Path, "/E-A0014-001"):
		list := "[]"
		if f.tsunami {
			list = `[{"TsunamiNo":7,"ReportNo":"1","ReportColor":"黃色","ReportType":"海嘯警報","ReportContent":"沿海注意",` +
				`"EarthquakeInfo":{"OriginTime":"` + f.tsunamiAge + `","Epicenter":{"Location":"東部海域"},"EarthquakeMagnitude":{"MagnitudeValue":7.1}}}]`
		}
		_, _ = fmt.Fprintf(w, `{"success":"true","records":{"Tsunami":%s}}`, list)
	default:
		http.NotFound(w, r)
	}
}

var checkNow = time.Date(2026, 11, 2, 21, 0, 0, 0, taipei)

func newTestChecker(t *testing.T, feed *cwaFeed, n *fakeNotifier) *Checker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(feed.handler))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "test-key"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	subs := &fakeSubs{subs: []alertsub.AlertSubscription{
		{UserID: "U1", AlertType: alertsub.TypeWeatherWarning, Regions: []string{"台北市"}, Enabled: true},
		{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
		{UserID: "U2", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
	}}
	c := NewChecker(subs, n, nil, cfg)
	c.now = func() time.Time { return checkNow }
	c.sent = newSentLogWithClock(nil, c.now)
	return c
}

func newNotifier() *fakeNotifier {
	return &fakeNotifier{attempts: map[string]int{}, failures: map[string]int{}}
}

func run(t *testing.T, c *Checker, times int) {
	t.Helper()
	for range times {
		if err := c.CheckAndNotify(context.Background()); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
}

// 每 5 分鐘檢查一次：同一則警報每個人只收到一次（以前是整則一個 key，重啟或 Redis 不在就全部重送）.
func TestEachAlertReachesEachSubscriberOnce(t *testing.T) {
	feed := &cwaFeed{warning: "特報", quakeTime: "2026-11-02 20:40:00"}
	n := newNotifier()
	c := newTestChecker(t, feed, n)

	run(t, c, 5)

	if got := n.count("U1", "[地震報告]"); got != 1 {
		t.Errorf("U1 earthquake pushes = %d, want 1", got)
	}
	if got := n.count("U2", "[地震報告]"); got != 1 {
		t.Errorf("U2 earthquake pushes = %d, want 1", got)
	}
	if got := n.count("U1", "[天氣特報] 豪雨"); got != 1 {
		t.Errorf("U1 warning pushes = %d, want 1", got)
	}
	if got := n.count("U2", "[天氣特報]"); got != 0 {
		t.Errorf("U2 did not subscribe to 台北市 warnings, got %d", got)
	}
}

// 資料源只給「最新一筆」地震；超過 3 小時的舊地震不能每天重推.
func TestOldEarthquakeIsNotPushed(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 17:30:00"} // 3.5 小時前
	n := newNotifier()
	c := newTestChecker(t, feed, n)
	run(t, c, 1)
	if len(n.pushes) != 0 {
		t.Errorf("old earthquake pushed: %v", n.pushes)
	}

	c.now = func() time.Time { return checkNow.Add(48 * time.Hour) } // 兩天後、紀錄也過了很久
	run(t, c, 1)
	if len(n.pushes) != 0 {
		t.Errorf("old earthquake pushed again later: %v", n.pushes)
	}
}

// 推播失敗只重試失敗的人；已經收到的人不會收到第二次.
func TestFailedPushIsRetriedOnlyForThatUser(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 20:40:00"}
	n := newNotifier()
	n.failures["U2"] = 1
	c := newTestChecker(t, feed, n)

	run(t, c, 1)
	if n.count("U1", "[地震報告]") != 1 || n.count("U2", "[地震報告]") != 0 {
		t.Fatalf("first run: %v", n.pushes)
	}
	run(t, c, 1)
	if n.count("U1", "[地震報告]") != 1 || n.count("U2", "[地震報告]") != 1 {
		t.Errorf("second run retries U2 only: %v", n.pushes)
	}
}

// 永久失敗的人最多試 maxPushTries 次，不會每 5 分鐘一直打 LINE API.
func TestPushRetriesStopAtTheLimit(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 20:40:00"}
	n := newNotifier()
	n.failures["U2"] = 100
	c := newTestChecker(t, feed, n)

	run(t, c, 10)

	if n.attempts["U2"] != maxPushTries {
		t.Errorf("attempts for a failing user = %d, want %d", n.attempts["U2"], maxPushTries)
	}
	if n.attempts["U1"] != 1 {
		t.Errorf("U1 attempts = %d", n.attempts["U1"])
	}
}

// 同一現象升級（特報 → 警報）要再通知一次.
func TestWarningUpgradeNotifiesAgain(t *testing.T) {
	feed := &cwaFeed{warning: "特報", quakeTime: "2026-11-02 10:00:00"}
	n := newNotifier()
	c := newTestChecker(t, feed, n)
	run(t, c, 2)
	feed.mu.Lock()
	feed.warning = "警報"
	feed.mu.Unlock()
	run(t, c, 2)
	if got := n.count("U1", "[天氣特報] 豪雨"); got != 2 {
		t.Errorf("warning pushes = %d, want 2 (one per level)", got)
	}
}

func TestTsunamiAgeAndGreenFilter(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 10:00:00", tsunami: true, tsunamiAge: "2026-11-02 20:55:00"}
	n := newNotifier()
	c := newTestChecker(t, feed, n)
	run(t, c, 3)
	if n.count("U1", "[海嘯警報]") != 1 || n.count("U2", "[海嘯警報]") != 1 {
		t.Errorf("recent tsunami goes to each subscriber once: %v", n.pushes)
	}
}

// 時間格式對不上時照常推一次（不能因為格式猜錯就完全不推），但不會重複.
func TestUnparsableTimeStillPushesOnce(t *testing.T) {
	feed := &cwaFeed{quakeTime: "11/02 20:40"}
	n := newNotifier()
	c := newTestChecker(t, feed, n)
	run(t, c, 3)
	if n.count("U1", "[地震報告]") != 1 {
		t.Errorf("pushes = %v", n.pushes)
	}
}

func TestTooOldAcceptsCWATimeFormats(t *testing.T) {
	c := &Checker{now: func() time.Time { return checkNow }}
	cases := map[string]bool{
		"2026-11-02 20:00:00":       false,
		"2026-11-02T20:00:00":       false,
		"2026-11-02T20:00:00+08:00": false,
		"2026-11-02 17:59:59":       true,
		"2026-11-01T21:00:00+08:00": true,
	}
	for in, want := range cases {
		if got := c.tooOld("earthquake", in); got != want {
			t.Errorf("tooOld(%q) = %v, want %v", in, got, want)
		}
	}
}

// slowFeed 地震資料要等測試放行才回（模擬檢查進行中）.
type slowFeed struct {
	*cwaFeed
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *slowFeed) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/E-A0015-001") {
		f.once.Do(func() { close(f.started) })
		<-f.release
	}
	f.cwaFeed.handler(w, r)
}

// 關機時等進行中的檢查推播完，不會推到一半就結束.
func TestSchedulerStopWaitsForTheRunningCheck(t *testing.T) {
	feed := &slowFeed{
		cwaFeed: &cwaFeed{quakeTime: time.Now().In(taipei).Add(-10 * time.Minute).Format("2006-01-02 15:04:05")},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(feed.handler))
	t.Cleanup(srv.Close)
	release := sync.OnceFunc(func() { close(feed.release) })
	t.Cleanup(release) // 測試失敗提早結束時也放行，否則 srv.Close 會一直等卡住的請求
	n := newNotifier()
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "test-key"},
		Alert: config.AlertConfig{
			WarningCron: "0 0 1 1 *", EarthquakeCron: "0 0 1 1 *", TsunamiCron: "0 0 1 1 *",
			WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001",
		},
	}
	subs := &fakeSubs{subs: []alertsub.AlertSubscription{{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true}}}
	loc, err := cwa.LoadTaipei(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewScheduler(NewChecker(subs, n, nil, cfg), cfg, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	<-feed.started

	done := make(chan error, 1)
	go func() { done <- s.Stop(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("stop returned during the check: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n.count("U1", "[地震報告]") != 1 {
		t.Errorf("the running check must finish its pushes: %v", n.pushes)
	}
}

// [F24-99] 縣市層級的特報也通知只訂了該縣某個鄉鎮的人.
func TestCountyWarningReachesTownshipSubscribers(t *testing.T) {
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = `{"success":"true","records":{"location":[{"locationName":"臺北市",` +
		`"hazardConditions":{"hazards":[{"info":{"phenomena":"濃霧","significance":"特報"}}]}}]}}`
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市", "U2": "台北市中正區", "U3": "新北市板橋區"}), n)
	if err := c.CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.count("U1", "濃霧") != 1 || n.count("U2", "濃霧") != 1 || n.count("U3", "") != 0 {
		t.Errorf("pushes: %v", n.pushes)
	}
}

// [F24-99] 沒有人開啟的通知類型不呼叫氣象署；縣市沒有訂閱者就不抓明細；查訂閱失敗也不呼叫.
func TestNoSubscribersNoCWACalls(t *testing.T) {
	quakeBodies := func(f *capFeed) {
		f.bodies["E-A0015-001"] = `{"success":"true","records":{"Earthquake":[]}}`
		f.bodies["E-A0014-001"] = `{"success":"true","records":{"Tsunami":[]}}`
		f.bodies["W-C0033-001"] = testdataBody(t, "W-C0033-001")
		f.bodies["W-C0033-006"] = testdataBody(t, "W-C0033-006")
	}
	quakeOnly := &fakeSubs{subs: []alertsub.AlertSubscription{{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true}}}
	for _, tc := range []struct {
		name    string
		subs    *fakeSubs
		wantErr bool
		check   func(t *testing.T, f *capFeed)
	}{
		{name: "nobody subscribed", subs: &fakeSubs{}, check: func(t *testing.T, f *capFeed) {
			if f.total() != 0 {
				t.Errorf("requests: %v", f.hits)
			}
		}},
		{name: "weather only", subs: weatherSubs(map[string]string{"U1": "台北市"}), check: func(t *testing.T, f *capFeed) {
			if f.count("E-A0015-001")+f.count("E-A0014-001") != 0 {
				t.Errorf("earthquake / tsunami fetched without subscribers: %v", f.hits)
			}
		}},
		{name: "earthquake only", subs: quakeOnly, check: func(t *testing.T, f *capFeed) {
			for ds := range f.hits {
				if strings.HasPrefix(ds, "W-C0033") {
					t.Errorf("weather warnings fetched without subscribers: %v", f.hits)
				}
			}
		}},
		{name: "no subscriber in the affected counties", subs: weatherSubs(map[string]string{"U1": "台北市"}), check: func(t *testing.T, f *capFeed) {
			if f.count("W-C0033-006") != 0 {
				t.Errorf("006 fetched although 台北市 has no strong wind: %v", f.hits)
			}
		}},
		{name: "subscription lookup fails", subs: &fakeSubs{err: errors.New("mongo down")}, wantErr: true, check: func(t *testing.T, f *capFeed) {
			if f.total() != 0 {
				t.Errorf("requests after a failed lookup: %v", f.hits)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feed := newCapFeed()
			quakeBodies(feed)
			err := newCapChecker(t, feed, tc.subs, newNotifier()).CheckAndNotify(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			tc.check(t, feed)
		})
	}
}
