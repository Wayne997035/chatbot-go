package cwa

import (
	"context"
	"testing"
	"time"
)

// [F36-12] 25 格窗口釘測：寫入的那個整點格要到 H+25 整點才滾出視窗，所以寫入後約 24 小時 58 分才歸零.
// 12:02 寫入（H = 12 點那格）；+24h57m = 隔天 12:59 仍是 H+24 整點格 → 還在；再 +1m = 13:00 是 H+25 → 歸零.
// 視窗少一格（24 格）→ 前者 0 而紅；多一格（26 格）→ 後者 1 而紅.
func TestLedgerWindowRollsOutAfter25Cells(t *testing.T) {
	captureLogs(t)
	clk := newTestClock(t)
	l := newMemLedger(t, clk, testSettings(nil))
	ctx := context.Background()
	if err := l.reserve(ctx, CallSpec{Stream: StreamEarthquake, Exempt: true}, 1); err != nil {
		t.Fatalf("exempt first attempt must pass: %v", err)
	}
	clk.Advance(24*time.Hour + 57*time.Minute)
	if got := l.Usage(ctx).Total; got != 1 {
		t.Fatalf("24h57m after the write the H+24 cell is still in the window: Total = %d, want 1", got)
	}
	clk.Advance(time.Minute)
	if got := l.Usage(ctx).Total; got != 0 {
		t.Fatalf("24h58m after the write the cell rolls out at H+25: Total = %d, want 0", got)
	}
}
