package alert

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// 天氣特報持續期間每人只推一次；解除超過 2 小時再發布（例如一週內第二波同等級寒流）要再推；
// 解除不到 2 小時又發布算同一則，不重推.
func TestWarningReissuedAfterLiftIsPushedAgain(t *testing.T) {
	const user = "U1"
	fog := warningsWith(t, "臺北市", "濃霧")
	setup := func(t *testing.T) (*capFeed, *fakeNotifier, *fakeClock, func(d time.Duration)) {
		t.Helper()
		clk := newFakeClock()
		feed := newCapFeed()
		feed.bodies["W-C0033-001"] = fog
		feed.status["W-C0033-006"] = http.StatusInternalServerError // 強風明細抓不到 → 退回縣市
		n := newNotifier()
		c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "台北市"}), n)
		useClock(c, clk)
		check := func(d time.Duration) {
			t.Helper()
			clk.advance(d)
			if err := c.CheckAndNotify(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		return feed, n, clk, check
	}

	t.Run("ongoing for hours, lifted, issued again", func(t *testing.T) {
		feed, n, _, check := setup(t)
		check(0)
		for range 3 { // 持續 3 小時（超過標記的 2 小時，靠每次看到續期）
			check(time.Hour)
		}
		if got := n.count(user, "濃霧"); got != 1 {
			t.Fatalf("ongoing warning pushed %d times, want 1: %v", got, n.pushes)
		}
		feed.set("W-C0033-001", noWarnings, 0) // 解除
		check(time.Hour)
		check(2 * time.Hour)
		feed.set("W-C0033-001", fog, 0) // 再發布
		check(5 * time.Minute)
		if got := n.count(user, "濃霧"); got != 2 {
			t.Errorf("warning issued again after the lift pushed %d times in total, want 2: %v", got, n.pushes)
		}
	})

	t.Run("township detail unavailable, notified by county", func(t *testing.T) {
		wind := warningsWith(t, "臺北市", "陸上強風")
		feed, n, _, check := setup(t)
		feed.set("W-C0033-001", wind, 0)
		check(time.Minute)
		check(time.Hour)
		feed.set("W-C0033-001", noWarnings, 0)
		check(time.Hour)
		check(2 * time.Hour)
		feed.set("W-C0033-001", wind, 0)
		check(5 * time.Minute)
		if got := n.count(user, "陸上強風"); got != 2 {
			t.Errorf("pushed %d times in total, want 2: %v", got, n.pushes)
		}
	})

	t.Run("lifted under 2 hours counts as the same warning", func(t *testing.T) {
		feed, n, _, check := setup(t)
		check(0)
		feed.set("W-C0033-001", noWarnings, 0)
		check(time.Hour)
		feed.set("W-C0033-001", fog, 0)
		check(5 * time.Minute)
		if got := n.count(user, "濃霧"); got != 1 {
			t.Errorf("pushed %d times, want 1", got)
		}
	})
}

// 鄉鎮明細的特報（這裡用低溫）同樣：解除超過 2 小時再發布要再推.
func TestTownshipNoticeReissuedAfterLiftIsPushedAgain(t *testing.T) {
	const user = "U1"
	clk := newFakeClock()
	live := coldBody(t, "2026-11-04T08:00:00+08:00", "低溫黃色燈號")
	feed := newCapFeed()
	feed.bodies["W-C0033-001"] = noWarnings
	feed.bodies["W-C0033-004"] = live
	n := newNotifier()
	c := newCapChecker(t, feed, weatherSubs(map[string]string{user: "新竹縣關西鎮"}), n)
	useClock(c, clk)
	check := func(d time.Duration) {
		t.Helper()
		clk.advance(d)
		if err := c.CheckAndNotify(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	check(0)
	feed.set("W-C0033-004", coldBody(t, "2026-11-03T05:10:00+08:00", "低溫黃色燈號"), 0) // 到期 = 解除
	for range 5 {
		check(30 * time.Minute)
	}
	feed.set("W-C0033-004", live, 0)
	check(30 * time.Minute)
	if got := n.count(user, "低溫黃色燈號"); got != 2 {
		t.Errorf("pushed %d times in total, want 2: %v", got, n.pushes)
	}
}

// 進行中標記：同一 key 持續續期就維持同一個開始時間；過期後換新.
func TestEpisodeStartIsKeptWhileRefreshed(t *testing.T) {
	clk := newFakeClock()
	e := newEpisodesWithClock(nil, clk.now)
	ctx := context.Background()
	first := e.start(ctx, "k", clk.now())
	for range 4 {
		clk.advance(time.Hour)
		if got := e.start(ctx, "k", clk.now()); got != first {
			t.Fatalf("refreshed episode changed start: %q -> %q", first, got)
		}
	}
	if other := e.start(ctx, "other", clk.now()); other == first {
		t.Errorf("a different key must start its own episode")
	}
	clk.advance(episodeTTL + time.Minute)
	if got := e.start(ctx, "k", clk.now()); got == first {
		t.Errorf("expired episode must start again, still %q", got)
	}
}
