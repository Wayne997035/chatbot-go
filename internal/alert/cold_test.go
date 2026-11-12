package alert

import (
	"chatbot-go/internal/models"
	"context"
	"net/http"
	"testing"
	"time"
)

// noWarnings 001 沒有任何特報.
const noWarnings = `{"success":"true","records":{"location":[]}}`

// coldBody 真實 004（那則 3 月就過期了）改成指定到期時間與燈號.
func coldBody(t *testing.T, expires, level string) string {
	t.Helper()
	resp := loadCap(t, "W-C0033-004")
	info := &resp.Records.Info[0]
	info.Expires = expires
	for i := range info.Parameter {
		if info.Parameter[i].ValueName == "severity_level" {
			info.Parameter[i].Value = level
		}
	}
	return capJSON(t, resp)
}

// useClock 檢查器改用可前進的假時鐘（推播紀錄、進行中標記同一個時鐘）.
func useClock(c *Checker, clk *fakeClock) {
	c.now = clk.now
	c.sent = newSentLogWithClock(nil, clk.now)
	c.episodes = newEpisodesWithClock(nil, clk.now)
}

func (f *capFeed) set(ds, body string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[ds] = body
	f.status[ds] = status
}

// 低溫特報不在總表 001（產品說明 6-3-1 只列颱風、豪(大)雨、陸上強風、濃霧）：
// 有人開天氣特報就直接抓 004，照明細列的鄉鎮通知（列到的鄉鎮與訂閱整縣的人）.
func TestColdWarningNotifiesListedTownshipsWithoutWarningList(t *testing.T) {
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = noWarnings
	feed.bodies["W-C0033-004"] = coldBody(t, "2026-11-06T08:00:00+08:00", "低溫黃色燈號")
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{
		"U1": "新竹縣關西鎮", "U2": "新竹縣竹北市", "U3": "新竹縣", "U4": "苗栗縣三義鄉", "U5": "台北市",
	}), n)
	if err := c.CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.count("U1", "") != 1 || n.count("U1", "低溫(低溫黃色燈號)") != 1 || n.count("U1", "關西鎮") != 1 {
		t.Errorf("U1 (listed township): %v", n.pushes)
	}
	if n.count("U3", "") != 1 || n.count("U3", "尖石鄉") != 1 {
		t.Errorf("U3 (whole county): %v", n.pushes)
	}
	if n.count("U4", "") != 1 || n.count("U4", "三義鄉") != 1 {
		t.Errorf("U4 (listed township in another county): %v", n.pushes)
	}
	for _, u := range []string{"U2", "U5"} {
		if got := n.count(u, ""); got != 0 {
			t.Errorf("%s must not be notified, got %d", u, got)
		}
	}
	if got := feed.count("W-C0033-004"); got != 1 {
		t.Errorf("004 requested %d times, want 1", got)
	}
}

// 低溫明細每 30 分鐘最多抓一次；同一則重抓不重推，燈號升級再推.
func TestColdWarningFetchedEvery30MinutesAndNotRepeated(t *testing.T) {
	const user = "U1"
	clk := newFakeClock()
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = noWarnings
	feed.bodies["W-C0033-004"] = coldBody(t, "2026-11-04T08:00:00+08:00", "低溫黃色燈號")
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "新竹縣關西鎮"}), n)
	useClock(c, clk)

	step := func(d time.Duration, wantFetches, wantPushes int) {
		t.Helper()
		clk.advance(d)
		if err := c.CheckAndNotify(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := feed.count("W-C0033-004"); got != wantFetches {
			t.Errorf("after +%v: 004 requested %d times, want %d", d, got, wantFetches)
		}
		if got := n.count(user, ""); got != wantPushes {
			t.Errorf("after +%v: %d pushes, want %d: %v", d, got, wantPushes, n.pushes)
		}
	}
	step(0, 1, 1)
	step(5*time.Minute, 1, 1)  // 還不到 30 分鐘：不抓
	step(25*time.Minute, 2, 1) // 30 分鐘到了再抓；同一則不重推
	feed.set("W-C0033-004", coldBody(t, "2026-11-04T08:00:00+08:00", "低溫橙色燈號1"), 0)
	step(30*time.Minute, 3, 2) // 升級：再推
	if n.count(user, "低溫橙色燈號1") != 1 {
		t.Errorf("upgrade push: %v", n.pushes)
	}
}

// 有抓到就要通知：低溫明細抓失敗不等 30 分鐘，下一輪（5 分鐘後）重抓；總表抓失敗也照樣處理低溫.
func TestColdWarningFetchFailureRetriesNextCheck(t *testing.T) {
	const user = "U1"
	cold := coldBody(t, "2026-11-04T08:00:00+08:00", "低溫黃色燈號")

	t.Run("004 fails", func(t *testing.T) {
		clk := newFakeClock()
		feed := newCapFeed()
		feed.bodies["W-C0033-001"] = noWarnings
		feed.set("W-C0033-004", cold, http.StatusInternalServerError)
		n := newNotifier()
		c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "新竹縣關西鎮"}), n)
		useClock(c, clk)
		if err := c.CheckAndNotify(context.Background()); err != nil {
			t.Fatalf("a failed cold-warning fetch must not fail the check: %v", err)
		}
		if n.count(user, "") != 0 {
			t.Fatalf("nothing fetched, nothing pushed: %v", n.pushes)
		}
		feed.set("W-C0033-004", cold, 0)
		clk.advance(5 * time.Minute)
		if err := c.CheckAndNotify(context.Background()); err != nil {
			t.Fatal(err)
		}
		// [F30-40] 500 在同一輪內由共用層重試一次（2 次請求），下一輪再重抓 1 次 = 3.
		if got := feed.count("W-C0033-004"); got != 3 {
			t.Errorf("004 requested %d times, want 3 (1 retry in the first check + 1 on the next check)", got)
		}
		if n.count(user, "低溫") != 1 {
			t.Errorf("retried fetch must notify: %v", n.pushes)
		}
	})

	t.Run("001 fails", func(t *testing.T) {
		feed := newCapFeed()
		feed.status["W-C0033-001"] = http.StatusInternalServerError
		feed.bodies["W-C0033-004"] = cold
		n := newNotifier()
		c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "新竹縣關西鎮"}), n)
		useClock(c, newFakeClock())
		if err := c.CheckAndNotify(context.Background()); err == nil {
			t.Error("a failed 001 fetch must still be reported")
		}
		if n.count(user, "低溫") != 1 {
			t.Errorf("cold warning must not depend on 001: %v", n.pushes)
		}
	})
}

// 總表若列了低溫（文件說不會，防萬一），只照 004 通知一次，不再另推一則縣市層級的.
func TestColdIn001IsNotNotifiedTwice(t *testing.T) {
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = warningsWith(t, "新竹縣", "低溫")
	feed.bodies["W-C0033-004"] = coldBody(t, "2026-11-06T08:00:00+08:00", "低溫黃色燈號")
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{"U1": "新竹縣關西鎮", "U3": "新竹縣"}), n)
	if err := c.CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"U1", "U3"} {
		if n.count(u, "") != 1 || n.count(u, "低溫黃色燈號") != 1 {
			t.Errorf("%s must get exactly the township notice: %v", u, n.pushes)
		}
	}
}

// [F241-05] 低溫明細只列了以縣市名開頭、但鄉鎮認不得的地名 → 該縣市整縣通知，不靜默漏發.
func TestColdWarningUnknownTownshipNotifiesWholeCounty(t *testing.T) {
	resp := loadCap(t, "W-C0033-004")
	resp.Records.Info[0].Expires = "2026-11-06T08:00:00+08:00"
	resp.Records.Info[0].Area = []models.CWACapArea{{AreaDesc: "新北市火星區"}}
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = noWarnings
	feed.bodies["W-C0033-004"] = capJSON(t, resp)
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{
		"U1": "新北市", "U2": "新北市板橋區", "U3": "台北市",
	}), n)
	if err := c.CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"U1", "U2"} {
		if n.count(u, "") != 1 || n.count(u, "低溫") != 1 {
			t.Errorf("%s: want one county-level cold notice, got %v", u, n.pushes)
		}
	}
	if got := n.count("U3", ""); got != 0 {
		t.Errorf("U3 (other county) got %d, want 0", got)
	}
}
