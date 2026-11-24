package cwa

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// readFailHook [F36-11] 啟動（armed）後讓讀取（pipeline 內有 hgetall）失敗：delay=0 即時回錯，delay>0 先睡再回錯（模擬逾時）.
// 寫入（hincrby）照常成功. client / hello 是 go-redis 新連線初始化，包在第一個真正的 pipeline 裡，一律直接放行，
// 否則初始化會把注入點吃掉（理由同 ledger_test.go 的 slowPipelineHook）.
type readFailHook struct {
	armed *atomic.Bool
	delay time.Duration
	hits  *atomic.Int32
}

func (readFailHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (readFailHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h readFailHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if n := cmds[0].Name(); n == "client" || n == "hello" {
			return next(ctx, cmds)
		}
		if h.armed.Load() {
			for _, c := range cmds {
				if c.Name() == "hgetall" {
					h.hits.Add(1)
					time.Sleep(h.delay)
					return errors.New("injected read failure")
				}
			}
		}
		return next(ctx, cmds)
	}
}

// newReadFailLedger 建一個接 miniredis 的 Ledger，hook 預設關閉；已先 Ping 暖連線.
func newReadFailLedger(t *testing.T, delay time.Duration) (*miniredis.Miniredis, *Ledger, *fakeClock, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	mr, rdb := newRedis(t)
	armed, hits := &atomic.Bool{}, &atomic.Int32{}
	rdb.AddHook(readFailHook{armed: armed, delay: delay, hits: hits})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("warm-up ping: %v", err)
	}
	clk := newTestClock(t)
	return mr, NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now), clk, armed, hits
}

func quake() CallSpec { return CallSpec{Stream: StreamEarthquake, Exempt: true} }

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

// [F36-11] A1：讀失敗（錯誤與 2 秒逾時兩種）時用最後成功快照 + 其後成功寫入估用量，預報（第 3 級）被擋，不少算.
// 還原成「讀失敗只回記憶體」→ 預報被放行、Usage=0 而紅.
func TestLedgerReadFailUsesLastSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"error", 0},
		{"timeout", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			mr, l, clk, armed, hits := newReadFailLedger(t, tc.delay)
			ctx := context.Background()
			mr.HSet(usageKey(hourOf(clk)), StreamEarthquake, "900")
			if err := l.reserve(ctx, quake(), 1); err != nil { // 讀成功：base=900；寫成功：post=1.
				t.Fatalf("exempt first attempt must pass: %v", err)
			}
			armed.Store(true)
			start := time.Now()
			err := l.reserve(ctx, CallSpec{Stream: StreamForecast}, 1)
			if !errors.Is(err, ErrQuotaReserved) {
				t.Fatalf("forecast must be blocked by the snapshot estimate (900+1 used), got err = %v", err)
			}
			if el := time.Since(start); el >= 2*time.Second {
				t.Fatalf("reserve took %v, want under 2s", el)
			}
			if out := logs.String(); !strings.Contains(out, "reason=quota_reserved") {
				t.Fatalf("log must show reason=quota_reserved: %s", out)
			}
			if got := l.Usage(ctx).ByStream[StreamEarthquake]; got != 901 {
				t.Fatalf("Usage during read failure must show snapshot + post: earthquake = %d, want 901", got)
			}
			if hits.Load() < 1 {
				t.Fatal("read-failure hook never fired (false green)")
			}
		})
	}
}

// [F36-11] A2：讀失敗期間本行程已寫成功的筆（redisPost）與讀失敗後的記憶體筆都算得到；讀成功後 post 清空不重複計.
// 不記 redisPost → 第一次 Usage=0 而紅；讀成功不清 post → 第一次 Usage 變 5 而紅（見下方斷言）.
func TestLedgerReadFailCountsLocalWrites(t *testing.T) {
	captureLogs(t)
	_, l, _, armed, hits := newReadFailLedger(t, 0)
	ctx := context.Background()
	for range 3 {
		if err := l.reserve(ctx, quake(), 1); err != nil {
			t.Fatal(err)
		}
	}
	armed.Store(true)
	if got := l.Usage(ctx).ByStream[StreamEarthquake]; got != 3 {
		t.Fatalf("read failure must still count the 3 writes that reached Redis: earthquake = %d, want 3", got)
	}
	if err := l.reserve(ctx, quake(), 1); err != nil { // 讀失敗中：只進記憶體.
		t.Fatal(err)
	}
	if got := l.Usage(ctx).ByStream[StreamEarthquake]; got != 4 {
		t.Fatalf("the call made during read failure must be counted: earthquake = %d, want 4", got)
	}
	armed.Store(false)
	if got := l.Usage(ctx).Total; got != 4 {
		t.Fatalf("after recovery Total must be Redis 3 + memory 1 (no double count of post): Total = %d, want 4", got)
	}
	if hits.Load() < 1 {
		t.Fatal("read-failure hook never fired (false green)")
	}
}

// [F36-11] A3：行程從沒讀成功過又讀失敗 → 只擋第 3 級非免擋（預報、低溫），第 1-2 級（含特報鄉鎮明細 W-C0033-003）照常通過並記記憶體.
// 刪規則 → 前兩個放行而紅；誤擋 tier 2 → 第三個紅.
func TestLedgerColdReadFailDeniesLowTierOnly(t *testing.T) {
	logs := captureLogs(t)
	_, l, _, armed, hits := newReadFailLedger(t, 0)
	armed.Store(true)
	ctx := context.Background()
	for _, spec := range []CallSpec{{Stream: StreamForecast}, {Stream: StreamCold}} {
		if err := l.reserve(ctx, spec, 1); !errors.Is(err, ErrQuotaReserved) {
			t.Fatalf("%s must be blocked on a cold read failure, got err = %v", spec.Stream, err)
		}
	}
	if out := logs.String(); !strings.Contains(out, "reason=redis_unreadable") {
		t.Fatalf("log must show reason=redis_unreadable: %s", out)
	}
	pass := []struct {
		spec    CallSpec
		attempt int
	}{
		{CallSpec{Stream: StreamWarning, Dataset: "W-C0033-003"}, 1},
		{quake(), 2},
		{quake(), 1},
	}
	for _, p := range pass {
		if err := l.reserve(ctx, p.spec, p.attempt); err != nil {
			t.Fatalf("%s attempt %d must pass on a cold read failure, got err = %v", p.spec.Stream, p.attempt, err)
		}
	}
	u := l.Usage(ctx)
	if u.ByStream[StreamWarning] != 1 || u.ByStream[StreamEarthquake] != 2 {
		t.Fatalf("passed calls must be counted in memory: %+v", u)
	}
	if hits.Load() < 1 {
		t.Fatal("read-failure hook never fired (false green)")
	}
}

// [F36-11] A4：暖機成功讀過一次、之後讀失敗 → 估計低於上限時第 2、3 級都通過（不得一律拒絕）.
// 實作成「讀失敗一律拒絕」→ 紅.
func TestLedgerReadFailWarmDoesNotBlanketDeny(t *testing.T) {
	captureLogs(t)
	_, l, _, armed, hits := newReadFailLedger(t, 0)
	ctx := context.Background()
	if got := l.Usage(ctx).Total; got != 0 { // 暖機：讀成功（Redis 空），base 已知.
		t.Fatalf("warm-up read: Total = %d, want 0", got)
	}
	armed.Store(true)
	if err := l.reserve(ctx, CallSpec{Stream: StreamWarning, Dataset: "W-C0033-003"}, 1); err != nil {
		t.Fatalf("warning must pass under the cap on a warm read failure: %v", err)
	}
	if err := l.reserve(ctx, CallSpec{Stream: StreamForecast}, 1); err != nil {
		t.Fatalf("forecast must pass under the cap on a warm read failure: %v", err)
	}
	u := l.Usage(ctx)
	if u.ByStream[StreamWarning] != 1 || u.ByStream[StreamForecast] != 1 {
		t.Fatalf("passed calls must be counted: %+v", u)
	}
	if hits.Load() < 1 {
		t.Fatal("read-failure hook never fired (false green)")
	}
}

// [F36-11] A5：rdb == nil 時冷啟動規則不觸發，行為與現行相同（回歸守，非現行行為的正對照）.
// 規則漏掉 l.rdb != nil 條件 → 預報被擋而紅.
func TestLedgerNilRedisIgnoresReadFailRule(t *testing.T) {
	logs := captureLogs(t)
	l := newMemLedger(t, newTestClock(t), testSettings(nil))
	ctx := context.Background()
	for _, spec := range []CallSpec{{Stream: StreamForecast}, {Stream: StreamCold}} {
		if err := l.reserve(ctx, spec, 1); err != nil {
			t.Fatalf("%s must pass without Redis: %v", spec.Stream, err)
		}
	}
	if out := logs.String(); strings.Contains(out, "redis_unreadable") {
		t.Fatalf("no redis_unreadable log expected without Redis: %s", out)
	}
}
