package alert

import (
	"context"
	"testing"
)

// fakeErasure [F241-12] 以函式決定某人是否「最近刪除過資料」.
type fakeErasure struct {
	erased func(userID string) bool
}

func (f fakeErasure) ErasedRecently(_ context.Context, userID string) bool { return f.erased(userID) }

const (
	erasureTestU1Key = "earthquake:115001:U1"
	erasureTestU2Key = "earthquake:115001:U2"
)

// attemptsOf 讀 fakeNotifier 對某人的 Push 次數.
func attemptsOf(n *fakeNotifier, userID string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.attempts[userID]
}

// [F241-12] 佔位後、推播前查到刪除標記：不推、剛佔的鍵（Redis 與本地）刪掉，沒被刪的人照推.
func TestDeliverSkipsRecipientErasedAfterClaim(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 20:40:00"}
	for _, backend := range []string{"local", "redis"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			n := newNotifier()
			c := newTestChecker(t, feed, n)
			c.WithErasureCheck(fakeErasure{erased: func(u string) bool { return u == "U1" }})
			var f *erasureFakeRedis
			if backend == "redis" {
				f = newErasureFakeRedis(t)
				c.sent = newSentLogWithClock(f.client(), c.now)
			}

			run(t, c, 1)

			if got := n.count("U1", "[地震報告]"); got != 0 {
				t.Errorf("erased U1 must not be pushed, got %d", got)
			}
			if got := n.count("U2", "[地震報告]"); got != 1 {
				t.Errorf("U2 pushes = %d, want 1", got)
			}
			if f != nil {
				for _, k := range []string{"alert:sent:" + erasureTestU1Key, "alert:sent:" + erasureTestU1Key + ":tries"} {
					if f.has(k) {
						t.Errorf("fake redis must not hold %q, keys: %v", k, f.keys())
					}
				}
				if !f.has("alert:sent:" + erasureTestU2Key) {
					t.Errorf("U2 claim must be in fake redis, keys: %v", f.keys())
				}
				return
			}
			if !c.sent.claim(ctx, erasureTestU1Key) {
				t.Error("U1 local claim must have been deleted")
			}
			if c.sent.claim(ctx, erasureTestU2Key) {
				t.Error("U2 local claim must remain")
			}
		})
	}
}

// [F241-12] 推播途中完成刪除（Push 之後才查到標記）：佔位與重試計數都刪掉，不殘留.
func TestDeliverForgetsTriesWhenErasedDuringFailedPush(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 20:40:00"}
	for _, backend := range []string{"local", "redis"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			n := newNotifier()
			n.failures["U1"] = 1
			c := newTestChecker(t, feed, n)
			var f *erasureFakeRedis
			if backend == "redis" {
				f = newErasureFakeRedis(t)
				// 預放計數 2：第一次失敗後達上限，failed() 保留佔位，重查前 map 裡一定有 U1 的 :tries.
				f.data["alert:sent:"+erasureTestU1Key+":tries"] = "2"
				c.sent = newSentLogWithClock(f.client(), c.now)
			}
			c.WithErasureCheck(fakeErasure{erased: func(u string) bool {
				if u != "U1" || attemptsOf(n, "U1") == 0 {
					return false // 推播之前還沒刪除
				}
				if f != nil && (!f.has("alert:sent:"+erasureTestU1Key) || !f.has("alert:sent:"+erasureTestU1Key+":tries")) {
					t.Errorf("before recheck fake redis must hold U1 claim and tries, keys: %v", f.keys())
				}
				return true
			}})

			run(t, c, 1)

			if got := attemptsOf(n, "U1"); got != 1 {
				t.Fatalf("control: U1 push attempts = %d, want 1", got)
			}
			if f != nil {
				for _, k := range []string{"alert:sent:" + erasureTestU1Key, "alert:sent:" + erasureTestU1Key + ":tries"} {
					if f.has(k) {
						t.Errorf("fake redis must not hold %q, keys: %v", k, f.keys())
					}
				}
				return
			}
			// 本地：計數不殘留，兩輪 claim→failed 都要能 claim，第 3 次仍 true，failed 後才達上限.
			for round := 1; round <= 2; round++ {
				if !c.sent.claim(ctx, erasureTestU1Key) {
					t.Fatalf("round %d: claim must be true (claim and tries removed)", round)
				}
				c.sent.failed(ctx, erasureTestU1Key)
			}
			if !c.sent.claim(ctx, erasureTestU1Key) {
				t.Fatal("third claim must be true")
			}
			c.sent.failed(ctx, erasureTestU1Key)
			if c.sent.claim(ctx, erasureTestU1Key) {
				t.Error("claim after reaching maxPushTries must be false")
			}
		})
	}
}
