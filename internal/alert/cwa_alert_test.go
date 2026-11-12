package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// pushTimes 依推播順序取出每則地震報告的發生時間字串.
func pushTimes(n *fakeNotifier) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, p := range n.pushes {
		_, text, _ := strings.Cut(p, "|")
		first, _, _ := strings.Cut(strings.TrimPrefix(text, "[地震報告] "), "\n")
		out = append(out, first)
	}
	return out
}

// logLine 回傳 out 裡含 msg 的第一行（找不到回空字串）.
func logLine(out, msg string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	return ""
}

// [F30-42] 地震請求帶 format=JSON 與 limit=<設定的筆數>（預設 5）.
func TestEarthquakeRequestLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{
		{"default", 0, "5"},
		{"configured", 3, "3"},
		{"max", 50, "50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feed := newRouteFeed(t)
			c := newFeedChecker(t, feed, quakeSubs(), newNotifier(), func(cfg *config.Config) { cfg.Alert.EarthquakeLimit = tc.limit })
			if err := c.CheckEarthquakes(context.Background()); err != nil {
				t.Fatal(err)
			}
			q := feed.query("E-A0015-001")
			if q.Get("limit") != tc.want || q.Get("format") != "JSON" {
				t.Errorf("query = %v, want format=JSON limit=%s", q, tc.want)
			}
		})
	}
}

// [F30-42] 一次回多筆（順序故意打亂）：由舊到新各推一次；時間解析不了的最後處理；超過上限的最舊那筆不處理；再跑一輪不重推.
func TestEarthquakeTwoNewReportsDeliveredOnceInOrder(t *testing.T) {
	t.Run("shuffled with an unparsable time", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(
			quakeSpec{1, "2026-11-02 20:50:00"}, quakeSpec{2, "bad time"}, quakeSpec{3, "2026-11-02 20:30:00"}, quakeSpec{4, "2026-11-02 20:40:00"},
			quakeSpec{5, "2026-11-02 16:00:00"}, // 超過 3 小時：不推
		))
		n := newNotifier()
		c := newFeedChecker(t, feed, quakeSubs(), n, nil)
		for range 2 {
			if err := c.CheckEarthquakes(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"2026-11-02 20:30:00", "2026-11-02 20:40:00", "2026-11-02 20:50:00", "bad time"}
		if got := pushTimes(n); !slices.Equal(got, want) {
			t.Errorf("push order = %v, want %v (each once, oldest first)", got, want)
		}
	})
	t.Run("beyond the limit the oldest is dropped", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(
			quakeSpec{1, "2026-11-02 20:30:00"}, quakeSpec{2, "2026-11-02 20:00:00"}, quakeSpec{3, "2026-11-02 20:50:00"},
			quakeSpec{4, "2026-11-02 20:10:00"}, quakeSpec{5, "2026-11-02 20:40:00"}, quakeSpec{6, "2026-11-02 20:20:00"},
		))
		n := newNotifier()
		c := newFeedChecker(t, feed, quakeSubs(), n, nil) // limit 預設 5
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := []string{"2026-11-02 20:10:00", "2026-11-02 20:20:00", "2026-11-02 20:30:00", "2026-11-02 20:40:00", "2026-11-02 20:50:00"}
		if got := pushTimes(n); !slices.Equal(got, want) {
			t.Errorf("push order = %v, want %v", got, want)
		}
	})
}

const yellowTsunamiBody = `{"success":"true","records":{"Tsunami":[{"TsunamiNo":7,"ReportNo":"1","ReportColor":"黃色","ReportType":"海嘯警報",` +
	`"ReportContent":"沿海注意","EarthquakeInfo":{"OriginTime":"2026-11-02 20:40:00","Epicenter":{"Location":"東部海域"},` +
	`"EarthquakeMagnitude":{"MagnitudeValue":7.1}}}]}}`

// [F30-42] 抓到新地震（有人被推播）就立刻順便查一次海嘯；沒有新推播、或地震太舊都不查.
func TestNewEarthquakeTriggersTsunamiCheck(t *testing.T) {
	t.Run("new earthquake", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}))
		feed.set("E-A0014-001", yellowTsunamiBody)
		n := newNotifier()
		c := newFeedChecker(t, feed, quakeSubs(), n, nil)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 1 || n.count("U1", "[海嘯警報]") != 1 {
			t.Errorf("tsunami requests=%d pushes=%v, want 1 request and the tsunami push", feed.count("E-A0014-001"), n.pushes)
		}
		if err := c.CheckEarthquakes(context.Background()); err != nil { // 沒有新的：不再查
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 1 {
			t.Errorf("tsunami requests = %d after a round with nothing new, want still 1", feed.count("E-A0014-001"))
		}
	})
	t.Run("two new earthquakes trigger one tsunami check", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}, quakeSpec{115002, "2026-11-02 20:50:00"}))
		c := newFeedChecker(t, feed, quakeSubs(), newNotifier(), nil)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 1 {
			t.Errorf("tsunami requests = %d, want 1 for two new earthquakes", feed.count("E-A0014-001"))
		}
	})
	t.Run("earthquake older than 3 hours", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 17:30:00"}))
		c := newFeedChecker(t, feed, quakeSubs(), newNotifier(), nil)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 0 {
			t.Errorf("tsunami requests = %d, want 0", feed.count("E-A0014-001"))
		}
	})
	t.Run("no earthquake subscribers", func(t *testing.T) {
		feed := newRouteFeed(t)
		c := newFeedChecker(t, feed, &fakeSubs{}, newNotifier(), nil)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0015-001")+feed.count("E-A0014-001") != 0 {
			t.Errorf("CWA called without subscribers: %v", feed.hits)
		}
	})
}

// [F30-42] 同一個地震編號最多觸發一次：後來才訂閱的人又被推了這則地震，也不再為它查海嘯；新編號才再查.
func TestTsunamiTriggerDedupedPerEarthquakeNo(t *testing.T) {
	feed := newRouteFeed(t)
	feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}))
	subs := quakeSubs()
	n := newNotifier()
	c := newFeedChecker(t, feed, subs, n, nil)
	if err := c.CheckEarthquakes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if feed.count("E-A0014-001") != 1 {
		t.Fatalf("first earthquake: tsunami requests = %d, want 1", feed.count("E-A0014-001"))
	}

	subs.subs = append(subs.subs, alertsub.AlertSubscription{UserID: "U2", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true})
	if err := c.CheckEarthquakes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.count("U2", "[地震報告]") != 1 {
		t.Fatalf("U2 must receive the earthquake (a new push this round): %v", n.pushes)
	}
	if feed.count("E-A0014-001") != 1 {
		t.Errorf("same earthquake no. triggered again: tsunami requests = %d, want 1", feed.count("E-A0014-001"))
	}

	feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}, quakeSpec{115002, "2026-11-02 20:55:00"}))
	if err := c.CheckEarthquakes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if feed.count("E-A0014-001") != 2 {
		t.Errorf("a new earthquake no. must trigger: tsunami requests = %d, want 2", feed.count("E-A0014-001"))
	}
}

// [F30-42] 推播失敗的重試輪不再為同一個地震編號查海嘯；3 小時後（去重鍵到期）同編號再有新推播可再次觸發.
func TestTsunamiTriggerRetryAndExpiry(t *testing.T) {
	t.Run("failed push retried next round", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}))
		n := newNotifier()
		n.failures["U1"] = 1
		c := newFeedChecker(t, feed, quakeSubs(), n, nil)
		for range 2 { // 第 1 輪推播失敗、第 2 輪重試成功
			if err := c.CheckEarthquakes(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if n.count("U1", "[地震報告]") != 1 || feed.count("E-A0014-001") != 1 {
			t.Fatalf("pushes=%v tsunami requests=%d, want the retry delivered and 1 tsunami request", n.pushes, feed.count("E-A0014-001"))
		}
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}, quakeSpec{115002, "2026-11-02 20:55:00"}))
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 2 {
			t.Errorf("tsunami requests = %d, want 2 (A once, B once)", feed.count("E-A0014-001"))
		}
	})
	t.Run("same earthquake no. after the key expired", func(t *testing.T) {
		feed := newRouteFeed(t)
		feed.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}))
		subs := quakeSubs()
		c := newFeedChecker(t, feed, subs, newNotifier(), nil)
		clk := newFakeClock()
		c.trigger = localcache.NewNoEvictWithClock[struct{}]("alert-tsunami-trigger-test", maxAlertAge, tsunamiTriggerWarnAbove, clk.now)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		subs.subs = append(subs.subs, alertsub.AlertSubscription{UserID: "U2", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true})
		clk.advance(maxAlertAge + time.Minute)
		if err := c.CheckEarthquakes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if feed.count("E-A0014-001") != 2 {
			t.Errorf("tsunami requests = %d, want 2 (trigger key expired after 3h)", feed.count("E-A0014-001"))
		}
	})
}

// deniedChecker 台北市豪雨特報 + 額度吃緊（非免擋被擋）的檢查器.
func deniedChecker(t *testing.T, feed *routeFeed, n *fakeNotifier) *Checker {
	t.Helper()
	feed.set("W-C0033-001", rainWarningBody)
	c := newFeedChecker(t, feed, weatherAndQuakeSubs(), n, nil)
	return c.WithCWAClient(deniedClient(t, feed.srv.URL))
}

// [F30-43] 額度吃緊：特報總表與地震（免擋）照打，低溫與鄉鎮明細（受擋）不打.
func TestAlertFirstCallsExemptOthersGated(t *testing.T) {
	feed := newRouteFeed(t)
	c := deniedChecker(t, feed, newNotifier())
	if err := c.CheckWarnings(context.Background()); err != nil {
		t.Fatalf("gated calls being skipped must not fail the check: %v", err)
	}
	if err := c.CheckEarthquakes(context.Background()); err != nil {
		t.Fatal(err)
	}
	for ds, want := range map[string]int{"W-C0033-001": 1, "E-A0015-001": 1, "W-C0033-004": 0, "W-C0033-003": 0} {
		if got := feed.count(ds); got != want {
			t.Errorf("%s requests = %d, want %d", ds, got, want)
		}
	}
}

// [F30-43] 鄉鎮明細被額度擋下 → 退回縣市通知（不是不推），並記分類 quota_reserved.
func TestTownshipDetailDeniedFallsBackToCounty(t *testing.T) {
	logs := captureText(t)
	feed := newRouteFeed(t)
	n := newNotifier()
	c := deniedChecker(t, feed, n)
	if err := c.CheckWarnings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.count("U1", "[天氣特報] 豪雨\n地區：台北市") != 1 {
		t.Errorf("county-level push expected: %v", n.pushes)
	}
	line := logLine(logs.String(), "fetch township warnings, notifying by county")
	if !strings.Contains(line, "class=quota_reserved") || !strings.Contains(line, "dataset=W-C0033-003") {
		t.Errorf("township log line = %q", line)
	}
}

// [F30-43] 低溫明細的間隔取自設定（ColdIntervalMinutes），不再寫死 30 分鐘：間隔 60 分、每 5 分鐘跑一輪連跑 13 輪 → 恰 2 次.
func TestColdIntervalFromSettings(t *testing.T) {
	feed := newRouteFeed(t)
	c := newFeedChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市"}), newNotifier(),
		func(cfg *config.Config) { cfg.Alert.ColdIntervalMinutes = 60 })
	clk := newFakeClock()
	useClock(c, clk)
	for range 13 {
		if err := c.CheckWarnings(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk.advance(5 * time.Minute)
	}
	if got := feed.count("W-C0033-004"); got != 2 {
		t.Errorf("cold requests = %d, want 2 (minute 0 and minute 60)", got)
	}
	if got := feed.count("W-C0033-001"); got != 13 {
		t.Errorf("001 requests = %d, want 13", got)
	}
}

// [F30-44] 失敗 log 只留分類：不含位址、query、主機名與內層原文；非氣象署錯誤照印遮蔽後的訊息.
func TestFetchFailureLogsAreRedacted(t *testing.T) {
	leaks := []string{"127.0.0.1", "?format=", "opendata.cwa.gov.tw", "connection refused", "deadline exceeded"}
	hangClient := func(c *Checker, baseURL string) {
		c.WithCWAClient(optClient(cwa.ClientOptions{BaseURL: baseURL, AttemptTimeout: 40 * time.Millisecond}))
	}
	for _, tc := range []struct {
		name string
		msg  string
		run  func(t *testing.T) (*Scheduler, func(context.Context) error)
		want []string // 該行必須含
	}{
		{"alert check failed", "alert check failed", func(t *testing.T) (*Scheduler, func(context.Context) error) {
			feed := newRouteFeed(t)
			feed.hang["W-C0033-001"] = true
			c := newFeedChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市"}), newNotifier(), nil)
			hangClient(c, feed.srv.URL)
			return &Scheduler{checker: c, cfg: c.cfg}, c.CheckWarnings
		}, []string{"job=cwa-warning", "class=timeout"}},
		{"cold fetch", "fetch cold warnings, retrying next check", func(t *testing.T) (*Scheduler, func(context.Context) error) {
			feed := newRouteFeed(t)
			feed.hang["W-C0033-004"] = true
			c := newFeedChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市"}), newNotifier(), nil)
			hangClient(c, feed.srv.URL)
			return &Scheduler{checker: c, cfg: c.cfg}, c.CheckWarnings
		}, []string{"class=timeout"}},
		{"township fetch", "fetch township warnings, notifying by county", func(t *testing.T) (*Scheduler, func(context.Context) error) {
			feed := newRouteFeed(t)
			feed.set("W-C0033-001", rainWarningBody)
			feed.hang["W-C0033-003"] = true
			c := newFeedChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市"}), newNotifier(), nil)
			hangClient(c, feed.srv.URL)
			return &Scheduler{checker: c, cfg: c.cfg}, c.CheckWarnings
		}, []string{"class=timeout", "dataset=W-C0033-003"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureText(t)
			s, check := tc.run(t)
			s.runJob(context.Background(), jobWarning, time.Minute, check)
			out := logs.String()
			line := logLine(out, tc.msg)
			if line == "" {
				t.Fatalf("no %q line: %s", tc.msg, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("line %q lacks %q", line, w)
				}
			}
			for _, bad := range leaks {
				if strings.Contains(out, bad) {
					t.Errorf("log leaks %q: %s", bad, out)
				}
			}
		})
	}

	t.Run("non-CWA error keeps the redacted message", func(t *testing.T) {
		logs := captureText(t)
		feed := newRouteFeed(t)
		subs := &fakeSubs{err: errors.New("mongo down")}
		c := newFeedChecker(t, feed, subs, newNotifier(), nil)
		(&Scheduler{checker: c, cfg: c.cfg}).runJob(context.Background(), jobEarthquake, time.Minute, c.CheckEarthquakes)
		line := logLine(logs.String(), "alert check failed")
		if !strings.Contains(line, "error=") || strings.Contains(line, "class=") || !strings.Contains(line, "job=cwa-earthquake") {
			t.Errorf("line = %q, want error= and job= but no class=", line)
		}
	})
}
