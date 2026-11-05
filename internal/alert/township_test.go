package alert

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// capNow 抓 testdata 的時間（2026-11-05 13:44）之後不久：006 有效，003、004 已過期.
var capNow = time.Date(2026, 11, 5, 14, 0, 0, 0, taipei)

func loadCap(t *testing.T, dataset string) *models.CWACapResponse {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + dataset + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var resp models.CWACapResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func countTowns(m map[string][]capNotice) int {
	n := 0
	for _, notices := range m {
		for _, nt := range notices {
			n += len(nt.towns)
			if nt.countyWide {
				n++
			}
		}
	}
	return n
}

// [F24-99] 只採用有效的明細：過期、解除（urgency Past 或 headline「解除」）、到期時間解析不了的都不算.
func TestCapAreasSkipsExpiredAndCanceled(t *testing.T) {
	if got := capAreas(loadCap(t, "W-C0033-003"), capNow); len(got) != 0 {
		t.Errorf("003 (canceled, expired): %v", got)
	}
	if got := capAreas(loadCap(t, "W-C0033-004"), capNow); len(got) != 0 {
		t.Errorf("004 (expired in March): %v", got)
	}
	wind := capAreas(loadCap(t, "W-C0033-006"), capNow)
	if n := countTowns(wind); n != 38 {
		t.Errorf("006: %d areas, want 38", n)
	}
	if nb := wind["新北市"]; len(nb) != 1 || !slices.Equal(nb[0].towns, []string{"林口區", "八里區"}) || nb[0].level != "強風黃色燈號" {
		t.Errorf("006 新北市: %+v", nb)
	}

	t.Run("canceled with future expiry", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-003")
		resp.Records.Info[0].Expires = "2026-11-06T00:00:00+08:00"
		if got := capAreas(resp, capNow); len(got) != 0 {
			t.Errorf("Past + 解除 must be ignored: %v", got)
		}
	})
	t.Run("unparsable expiry", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-006")
		resp.Records.Info[0].Expires = "not-a-time"
		if got := capAreas(resp, capNow); len(got) != 0 {
			t.Errorf("an unparsable expiry must be treated as invalid: %v", got)
		}
	})
	t.Run("several valid notices", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-006")
		second := resp.Records.Info[0]
		second.Parameter = []models.CWACapParam{{ValueName: "severity_level", Value: "強風橙色燈號"}}
		second.Area = []models.CWACapArea{{AreaDesc: "新北市石門區"}}
		resp.Records.Info = append(resp.Records.Info, second)
		nb := capAreas(resp, capNow)["新北市"]
		if len(nb) != 2 ||
			!slices.Equal(nb[0].towns, []string{"林口區", "八里區"}) || nb[0].level != "強風黃色燈號" ||
			!slices.Equal(nb[1].towns, []string{"石門區"}) || nb[1].level != "強風橙色燈號" {
			t.Errorf("each valid notice is kept on its own: %+v", nb)
		}
	})
	t.Run("headline cancel only", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-003")
		resp.Records.Info[0].Expires = "2026-11-06T00:00:00+08:00"
		resp.Records.Info[0].Urgency = "Expected"
		if got := capAreas(resp, capNow); len(got) != 0 {
			t.Errorf("headline 解除 must be ignored: %v", got)
		}
	})
	t.Run("urgency past only", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-003")
		resp.Records.Info[0].Expires = "2026-11-06T00:00:00+08:00"
		resp.Records.Info[0].Headline = "大雨特報"
		if got := capAreas(resp, capNow); len(got) != 0 {
			t.Errorf("urgency Past must be ignored: %v", got)
		}
	})
	t.Run("county itself", func(t *testing.T) {
		resp := loadCap(t, "W-C0033-006")
		resp.Records.Info[0].Area = []models.CWACapArea{{AreaDesc: "臺北市"}}
		tp := capAreas(resp, capNow)["台北市"]
		if len(tp) != 1 || !tp[0].countyWide || len(tp[0].towns) != 0 {
			t.Errorf("a county-level area marks the whole county: %+v", tp)
		}
	})
}

// [F24-99] 001 的現象對應到有鄉鎮明細的資料集；其他現象照縣市通知. 低溫不在 001（由 004 直接處理）.
func TestPhenomenonDataset(t *testing.T) {
	for p, want := range map[string]string{
		"陸上強風": "W-C0033-006", "低溫": "",
		"大雨": "W-C0033-003", "豪雨": "W-C0033-003", "大豪雨": "W-C0033-003", "超大豪雨": "W-C0033-003",
		"濃霧": "", "": "",
	} {
		if got := phenomenonDataset(p); got != want {
			t.Errorf("phenomenonDataset(%q) = %q, want %q", p, got, want)
		}
	}
}

// capFeed 假氣象署：每份資料集回固定內容（沒設定的回 404），並記錄被請求幾次.
type capFeed struct {
	mu     sync.Mutex
	bodies map[string]string
	status map[string]int
	hits   map[string]int
}

func newCapFeed() *capFeed {
	return &capFeed{bodies: map[string]string{}, status: map[string]int{}, hits: map[string]int{}}
}

func (f *capFeed) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ds := path.Base(r.URL.Path)
	f.hits[ds]++
	if code := f.status[ds]; code != 0 {
		w.WriteHeader(code)
		return
	}
	body, ok := f.bodies[ds]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (f *capFeed) count(ds string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[ds]
}

func (f *capFeed) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.hits {
		n += v
	}
	return n
}

func newCapChecker(t *testing.T, feed *capFeed, subs *fakeSubs, n *fakeNotifier) *Checker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(feed.handler))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "test-key"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	c := NewChecker(subs, n, nil, cfg)
	c.now = func() time.Time { return capNow }
	c.sent = newSentLogWithClock(nil, c.now)
	return c
}

func weatherSubs(regions map[string]string) *fakeSubs {
	f := &fakeSubs{}
	for user, r := range regions {
		f.subs = append(f.subs, alertsub.AlertSubscription{UserID: user, AlertType: alertsub.TypeWeatherWarning, Regions: []string{r}, Enabled: true})
	}
	return f
}

func testdataBody(t *testing.T, dataset string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + dataset + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func capJSON(t *testing.T, resp *models.CWACapResponse) string {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// warningsWith 真實 001 裡把某縣市的特報換成指定現象（其他縣市不動）.
func warningsWith(t *testing.T, county, phenomena string) string {
	t.Helper()
	var resp models.CWAAlertResponse
	if err := json.Unmarshal([]byte(testdataBody(t, "W-C0033-001")), &resp); err != nil {
		t.Fatal(err)
	}
	for i := range resp.Records.Location {
		if resp.Records.Location[i].LocationName == county {
			resp.Records.Location[i].HazardCondition.Hazards = []models.CWAHazard{{Info: models.CWAHazardInfo{Phenomena: phenomena, Significance: "特報"}}}
		}
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// [F24-99] 強風照鄉鎮通知：只通知明細列出的鄉鎮與訂閱整縣的人；同一輪明細只抓一次.
func TestStrongWindNotifiesOnlyListedTownships(t *testing.T) {
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = testdataBody(t, "W-C0033-001")
	feed.bodies["W-C0033-006"] = testdataBody(t, "W-C0033-006")
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{
		"U1": "新北市林口區", "U2": "新北市板橋區", "U3": "新北市", "U4": "台北市", "U5": "台中市大甲區", "U6": "台中市沙鹿區",
	}), n)
	if err := c.CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.count("U1", "") != 1 || n.count("U1", "強風黃色燈號") != 1 || n.count("U1", "林口區") != 1 {
		t.Errorf("U1 (listed township): %v", n.pushes)
	}
	if n.count("U3", "") != 1 || n.count("U3", "林口區") != 1 || n.count("U3", "八里區") != 1 {
		t.Errorf("U3 (whole county): %v", n.pushes)
	}
	if n.count("U5", "") != 1 || n.count("U5", "大甲區") != 1 {
		t.Errorf("U5 (listed township in another county): %v", n.pushes)
	}
	for _, u := range []string{"U2", "U4", "U6"} {
		if got := n.count(u, ""); got != 0 {
			t.Errorf("%s must not be notified, got %d", u, got)
		}
	}
	if got := feed.count("W-C0033-006"); got != 1 {
		t.Errorf("006 requested %d times, want 1 (shared by 新北市 and 台中市)", got)
	}
}

// [F24-99] 明細抓不到、只有過期的、沒列本縣、只列了不認得的鄉鎮 → 退回縣市通知，不漏發.
func TestTownshipDetailFallsBackToCounty(t *testing.T) {
	const user = "Ufallback00000000000000000000002"
	onlyArea := func(desc string) string {
		resp := loadCap(t, "W-C0033-006")
		resp.Records.Info[0].Area = []models.CWACapArea{{AreaDesc: desc}}
		return capJSON(t, resp)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "fetch fails", status: http.StatusInternalServerError},
		{name: "only expired notices", body: testdataBody(t, "W-C0033-004")},
		{name: "county not listed", body: onlyArea("臺中市清水區")},
		{name: "unknown township", body: onlyArea("新北市火星區")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			feed := newCapFeed()
			feed.bodies["W-C0033-001"] = testdataBody(t, "W-C0033-001")
			if tc.status != 0 {
				feed.status["W-C0033-006"] = tc.status
			} else {
				feed.bodies["W-C0033-006"] = tc.body
			}
			n := newNotifier()
			c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "新北市板橋區"}), n)
			if err := c.CheckAndNotify(context.Background()); err != nil {
				t.Fatalf("a failed detail fetch must not fail the check: %v", err)
			}
			if n.count(user, "") != 1 || n.count(user, "陸上強風") != 1 {
				t.Errorf("county-level fallback: %v", n.pushes)
			}
			if strings.Contains(buf.String(), "火星區") || strings.Contains(buf.String(), user) {
				t.Errorf("log must not carry place names from the feed or the userID: %s", buf.String())
			}
		})
	}
}

// [F24-99] 豪大雨：明細沒有 severity_level 時等級字串用 headline；headline 不是 001 說的那種雨就照縣市通知.
func TestWeatherWarning003Detail(t *testing.T) {
	rain := capJSON(t, &models.CWACapResponse{Success: "true", Records: models.CWACapRecords{Info: []models.CWACapInfo{{
		Urgency: "Expected", Expires: "2026-11-06T00:00:00+08:00", Headline: "大雨特報",
		Parameter: []models.CWACapParam{{ValueName: "alert_title", Value: "大雨特報"}},
		Area:      []models.CWACapArea{{AreaDesc: "臺北市士林區"}},
	}}}})
	subs := map[string]string{"U1": "台北市士林區", "U2": "台北市中正區", "U3": "台北市"}

	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = warningsWith(t, "臺北市", "大雨")
	feed.bodies["W-C0033-003"] = rain
	n := newNotifier()
	if err := newCapChecker(t, feed, weatherSubs(subs), n).CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"U1", "U3"} {
		if n.count(u, "") != 1 || n.count(u, "大雨(大雨特報)") != 1 || n.count(u, "士林區") != 1 || n.count(u, "()") != 0 {
			t.Errorf("%s: %v", u, n.pushes)
		}
	}
	if got := n.count("U2", ""); got != 0 {
		t.Errorf("U2 (unlisted township) got %d", got)
	}

	t.Run("headline names another rain level", func(t *testing.T) {
		feed := newCapFeed()
		feed.bodies["W-C0033-001"] = warningsWith(t, "臺北市", "豪雨")
		feed.bodies["W-C0033-003"] = rain
		n := newNotifier()
		if err := newCapChecker(t, feed, weatherSubs(subs), n).CheckAndNotify(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{"U1", "U2", "U3"} {
			if n.count(u, "") != 1 || n.count(u, "士林區") != 0 {
				t.Errorf("%s: want one county-level notice, got %v", u, n.pushes)
			}
		}
	})
}
