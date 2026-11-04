package webhook

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// erasureStart 帶不足 1ms 的奈秒殘值，讓「不做毫秒截斷」的實作測得出來.
func erasureStart() time.Time {
	return time.Date(2026, 11, 4, 12, 0, 0, 500_000, time.UTC)
}

// TestErasuresBlocksEarlierSend [F24-33] 刪除前送出的訊息被擋.
func TestErasuresBlocksEarlierSend(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(nil)
	e.now = clk.now
	ctx := context.Background()
	e.mark(ctx, "U1")
	markedAt := clk.now().Truncate(time.Millisecond)
	tests := []struct {
		name   string
		sentAt time.Time
		want   bool
	}{
		{"1ms before mark", markedAt.Add(-time.Millisecond), true},
		{"1s before mark", markedAt.Add(-time.Second), true},
		{"1ms after mark", markedAt.Add(time.Millisecond), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.erasedSince(ctx, "U1", tt.sentAt); got != tt.want {
				t.Errorf("erasedSince = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestErasuresSameMillisecondNotBlocked [F24-33] 同一毫秒送出的訊息不被誤擋（mark 截到毫秒）.
func TestErasuresSameMillisecondNotBlocked(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(nil)
	e.now = clk.now
	ctx := context.Background()
	sentAt := clk.now().Truncate(time.Millisecond) // LINE 事件時間只到毫秒
	e.mark(ctx, "U1")
	if e.erasedSince(ctx, "U1", sentAt) {
		t.Error("message sent in the same millisecond as mark was blocked")
	}
	if e.erasedSince(ctx, "U1", sentAt.Add(time.Millisecond)) {
		t.Error("message sent after mark was blocked")
	}
}

// TestErasuresExpireAfterTTL [F24-33] 超過 erasureTTL 後不再擋.
func TestErasuresExpireAfterTTL(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(nil)
	e.now = clk.now
	ctx := context.Background()
	e.mark(ctx, "U1")
	sentAt := clk.now().Add(-time.Second)
	clk.advance(erasureTTL - time.Second)
	if !e.erasedSince(ctx, "U1", sentAt) {
		t.Error("blocked = false just before TTL, want true")
	}
	clk.advance(2 * time.Second)
	if e.erasedSince(ctx, "U1", sentAt) {
		t.Error("blocked = true after TTL, want false")
	}
}

// TestErasuresPerUserAndNilReceiver [F24-33] 標記依使用者區分；nil receiver 安全.
func TestErasuresPerUserAndNilReceiver(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(nil)
	e.now = clk.now
	ctx := context.Background()
	e.mark(ctx, "U1")
	earlier := clk.now().Add(-time.Second)
	if !e.erasedSince(ctx, "U1", earlier) {
		t.Error("U1 not blocked after mark")
	}
	if e.erasedSince(ctx, "U2", earlier) {
		t.Error("U2 blocked by U1's erasure")
	}
	var nilE *erasures
	nilE.mark(ctx, "U1")
	if nilE.erasedSince(ctx, "U1", earlier) {
		t.Error("nil receiver reported erased")
	}
}

// TestErasuresLocalWinsWhenRedisDown [F24-33] Redis 連不上時本地標記仍生效.
func TestErasuresLocalWinsWhenRedisDown(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(unreachableRedis(t))
	e.now = clk.now
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.mark(ctx, "U1")
	if !e.erasedSince(ctx, "U1", clk.now().Add(-time.Second)) {
		t.Error("erasure lost when Redis is down, want local mark to win")
	}
}

// TestErasuresKeepAllUsers [F24-33] 大量使用者的標記全部保留（不淘汰、不拒收）.
func TestErasuresKeepAllUsers(t *testing.T) {
	clk := newTestClock(erasureStart())
	e := newErasures(nil)
	e.now = clk.now
	ctx := context.Background()
	const users = 2000
	for i := 0; i < users; i++ {
		e.mark(ctx, fmt.Sprintf("U%d", i))
	}
	earlier := clk.now().Add(-time.Second)
	lost := 0
	for i := 0; i < users; i++ {
		if !e.erasedSince(ctx, fmt.Sprintf("U%d", i), earlier) {
			lost++
		}
	}
	if lost != 0 {
		t.Errorf("%d of %d erasure marks lost, want 0", lost, users)
	}
}
