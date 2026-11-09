package alert

import (
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeErasure [F241-12] 以函式決定某人是否「最近刪除過資料」.
type fakeErasure struct {
	erased func(userID string) bool
}

func (f fakeErasure) ErasedSince(_ context.Context, userID string, _ time.Time) bool {
	return f.erased(userID)
}

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

// cancelOnPush [F242-01] Push 時先取消本輪 ctx（模擬推播中途逾時或關機）、記下已嘗試，再回失敗.
type cancelOnPush struct {
	cancel    context.CancelFunc
	attempted atomic.Bool
	calls     atomic.Int32
}

func (p *cancelOnPush) Push(_ context.Context, _, _ string) error {
	p.cancel()
	p.attempted.Store(true)
	p.calls.Add(1)
	return errors.New("push failed")
}

// ctxErasure [F242-01] U1 且已嘗試推播且 ctx 未取消才回 true（模擬 Redis GET 綁在已取消的 ctx 上就讀不到刪除標記）.
type ctxErasure struct{ pushed *cancelOnPush }

func (e ctxErasure) ErasedSince(ctx context.Context, userID string, _ time.Time) bool {
	return userID == "U1" && e.pushed.attempted.Load() && ctx.Err() == nil
}

// detachedErasure [F242-01] U1 且 ctx.Done() == nil（不會被取消）才回 true.
type detachedErasure struct{}

func (detachedErasure) ErasedSince(ctx context.Context, userID string, _ time.Time) bool {
	return userID == "U1" && ctx.Done() == nil
}

// sinceErasure [F242-02] 依 since 判斷：刪除時間晚於 since 才算刪除過.
type sinceErasure struct{ erasedAt time.Time }

func (s sinceErasure) ErasedSince(_ context.Context, userID string, since time.Time) bool {
	if userID != "U1" && userID != "U2" {
		return false
	}
	return s.erasedAt.After(since)
}

// TestDeliverRecheckAfterFailedPushIgnoresRoundCancel [F242-01] 推播失敗後的重查不受本輪取消影響：
// Push 途中 ctx 被取消，重查若綁在這個 ctx 上就讀不到刪除標記，剛寫的重試紀錄會殘留.
func TestDeliverRecheckAfterFailedPushIgnoresRoundCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newTestChecker(t, &cwaFeed{}, newNotifier())
	p := &cancelOnPush{cancel: cancel}
	c.notifier = p
	c.WithErasureCheck(ctxErasure{pushed: p})
	f := newErasureFakeRedis(t)
	f.data["alert:sent:"+erasureTestU1Key+":tries"] = "2"
	c.sent = newSentLogWithClock(f.client(), c.now)

	c.deliver(ctx, "earthquake", "115001", "[地震報告] test", []alertsub.AlertSubscription{{UserID: "U1"}})

	if got := p.calls.Load(); got != 1 {
		t.Fatalf("control: push attempts = %d, want 1", got)
	}
	for _, k := range []string{"alert:sent:" + erasureTestU1Key, "alert:sent:" + erasureTestU1Key + ":tries"} {
		if f.has(k) {
			t.Errorf("fake redis must not hold %q, keys: %v", k, f.keys())
		}
	}
}

// TestDeliverPrePushErasureCheckIgnoresRoundCancel [F242-01] 推播前的重查不受本輪取消影響.
// context.WithoutCancel 回傳的 ctx 其 Done() 為 nil（標準庫 context.go withoutCancelCtx.Done 回 nil），
// 這樣才能不靠計時、確定地驗證重查沒有綁在本輪取消上；真實情況是迴圈開頭檢查完 ctx 之後、重查之前剛好逾時.
func TestDeliverPrePushErasureCheckIgnoresRoundCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNotifier()
	c := newTestChecker(t, &cwaFeed{}, n)
	c.WithErasureCheck(detachedErasure{})

	c.deliver(ctx, "earthquake", "115001", "[地震報告] test", []alertsub.AlertSubscription{{UserID: "U1"}, {UserID: "U2"}})

	if got := attemptsOf(n, "U1"); got != 0 {
		t.Errorf("erased U1 push attempts = %d, want 0", got)
	}
	if got := attemptsOf(n, "U2"); got != 1 {
		t.Errorf("U2 push attempts = %d, want 1", got)
	}
}

// TestDeliverPushesSubscriptionCreatedAfterErasure [F242-02] 推播前重查以訂閱的更新時間當切點：
// 刪除之後才重新訂閱的人照推，刪除之前的訂閱不推.
func TestDeliverPushesSubscriptionCreatedAfterErasure(t *testing.T) {
	erasedAt := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	subs := []alertsub.AlertSubscription{
		{UserID: "U1", UpdateTime: erasedAt.Add(time.Minute)},
		{UserID: "U2", UpdateTime: erasedAt.Add(-time.Hour)},
	}
	n := newNotifier()
	c := newTestChecker(t, &cwaFeed{}, n)
	c.WithErasureCheck(sinceErasure{erasedAt: erasedAt})

	c.deliver(context.Background(), "earthquake", "115001", "[地震報告] test", subs)

	if got := attemptsOf(n, "U1"); got != 1 {
		t.Errorf("resubscribed U1 push attempts = %d, want 1", got)
	}
	if got := attemptsOf(n, "U2"); got != 0 {
		t.Errorf("U2 (subscribed before erasure) push attempts = %d, want 0", got)
	}
}

// TestDeliverKeepsResubscriberRetryAfterFailedPush [F242-02] 推播失敗後的重查也以訂閱的更新時間當切點：
// 重新訂閱者的重試紀錄照 maxPushTries 規則保留，不被當成刪除過的人清掉.
func TestDeliverKeepsResubscriberRetryAfterFailedPush(t *testing.T) {
	erasedAt := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	n := newNotifier()
	n.failures["U1"] = 1
	c := newTestChecker(t, &cwaFeed{}, n)
	f := newErasureFakeRedis(t)
	f.data["alert:sent:"+erasureTestU1Key+":tries"] = "2"
	c.sent = newSentLogWithClock(f.client(), c.now)
	c.WithErasureCheck(sinceErasure{erasedAt: erasedAt})
	subs := []alertsub.AlertSubscription{{UserID: "U1", UpdateTime: erasedAt.Add(time.Minute)}}

	c.deliver(context.Background(), "earthquake", "115001", "[地震報告] test", subs)

	if got := attemptsOf(n, "U1"); got != 1 {
		t.Fatalf("control: U1 push attempts = %d, want 1", got)
	}
	for _, k := range []string{"alert:sent:" + erasureTestU1Key, "alert:sent:" + erasureTestU1Key + ":tries"} {
		if !f.has(k) {
			t.Errorf("fake redis must still hold %q, keys: %v", k, f.keys())
		}
	}
}
