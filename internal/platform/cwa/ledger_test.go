package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// F30-33：每次嘗試都記，重試另計 :retry，003 記為 warning.
func TestLedgerRecordsEveryAttempt(t *testing.T) {
	captureLogs(t)
	mr, rdb := newRedis(t)
	clk := newTestClock(t)
	s := testSettings(nil)
	l := NewLedger(rdb, s, taipeiLoc(t), clk.Now)
	ts := newTestServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		// 地震的第一次請求失敗 → 重試.
		if strings.Contains(r.URL.Path, "E-A0015") && n > 0 && !strings.Contains(r.Header.Get("X-Seen"), "1") {
			r.Header.Set("X-Seen", "1")
		}
		okJSON(w)
	})
	quakeFailedOnce := false
	flaky := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		if !quakeFailedOnce {
			quakeFailedOnce = true
			w.WriteHeader(503)
			return
		}
		okJSON(w)
	})
	call := func(srv *testServer, spec CallSpec, path string) {
		t.Helper()
		var out decoded
		c := newTestClient(srv, l, s, nil)
		if err := c.GetJSON(context.Background(), spec, path, nil, &out); err != nil {
			t.Fatalf("%s: %v", spec.Stream, err)
		}
	}
	call(ts, CallSpec{Stream: StreamWarning, Exempt: true}, "W-C0033-001")
	call(ts, CallSpec{Stream: StreamWarning}, "W-C0033-003") // 鄉鎮明細也是 warning.
	call(flaky, CallSpec{Stream: StreamEarthquake, Exempt: true}, "E-A0015-001")
	call(ts, CallSpec{Stream: StreamTsunami, Exempt: true}, "E-A0014-001")
	call(ts, CallSpec{Stream: StreamCold}, "W-C0033-004")
	call(ts, CallSpec{Stream: StreamForecast}, "F-D0047-003")

	key := "cwa:usage:" + strconv.FormatInt(hourOf(clk), 10)
	want := map[string]string{
		"warning": "2", "earthquake": "2", "earthquake:retry": "1", "tsunami": "1", "cold": "1", "forecast": "1",
	}
	for field, v := range want {
		if got := mr.HGet(key, field); got != v {
			t.Errorf("%s = %q, want %q", field, got, v)
		}
	}
	if got := mr.HGet(key, "warning:retry"); got != "" {
		t.Errorf("warning:retry = %q, want unset", got)
	}
	u := l.Usage(context.Background())
	if u.Total != 7 || u.ByStream["earthquake"] != 2 || u.ByStream["warning"] != 2 {
		t.Fatalf("Usage = %+v (retry must not double count)", u)
	}
}

// F30-33：Redis 不可用退記憶體；不影響抓取；不重複計算.
func TestLedgerFallsBackToMemory(t *testing.T) {
	ctx := context.Background()
	ok := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
	fetch := func(t *testing.T, l *Ledger) {
		t.Helper()
		var out decoded
		if err := newTestClient(ok, l, testSettings(nil), nil).GetJSON(ctx, warnSpec, "x", nil, &out); err != nil || !out.OK {
			t.Fatalf("bookkeeping must never fail the fetch: %v", err)
		}
	}
	t.Run("nil redis client", func(t *testing.T) {
		captureLogs(t)
		l := NewLedger(nil, testSettings(nil), taipeiLoc(t), newTestClock(t).Now)
		fetch(t, l)
		if got := l.Usage(ctx).ByStream[StreamWarning]; got != 1 {
			t.Fatalf("usage = %d", got)
		}
	})
	t.Run("redis down then recovered", func(t *testing.T) {
		logs := captureLogs(t)
		mr, rdb := newRedis(t)
		clk := newTestClock(t)
		l := NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now)
		mr.Close()
		fetch(t, l)
		if got := l.Usage(ctx).Total; got != 1 {
			t.Fatalf("memory fallback not visible in Usage: %d", got)
		}
		if out := logs.String(); !strings.Contains(out, "falling back to memory") || strings.Contains(out, "127.0.0.1") {
			t.Fatalf("want one address-free warn, got: %s", out)
		}
		if strings.Count(logs.String(), "falling back to memory") != 1 {
			t.Fatalf("warn must be at most once per hour: %s", logs)
		}
		if err := mr.Restart(); err != nil {
			t.Fatal(err)
		}
		fetch(t, l)
		if got := l.Usage(ctx).Total; got != 2 {
			t.Fatalf("after recovery Total = %d, want 2 (no double counting)", got)
		}
		if got := mr.HGet("cwa:usage:"+strconv.FormatInt(hourOf(clk), 10), "warning"); got != "1" {
			t.Fatalf("redis holds %q, want only the post-recovery request", got)
		}
	})
}

// F30-33：視窗 = 25 個整點格；Total 不重複計 :retry；五個 stream 鍵都在.
func TestUsageWindow(t *testing.T) {
	ctx := context.Background()
	// 台灣時間跨午夜（00:30）：視窗是滾動格，不是日曆日.
	clk := &fakeClock{t: time.Date(2026, 11, 11, 0, 30, 0, 0, taipeiLoc(t))}
	l := newMemLedger(t, clk, testSettings(func(c *config.Config) { c.CWA.DailyLimit = 777 }))
	h0 := hourOf(clk)
	seed(l, h0-25, map[string]int{StreamWarning: 1000}) // 第 26 格：不計.
	seed(l, h0-24, map[string]int{StreamWarning: 3, StreamEarthquake: 1, StreamEarthquake + ":retry": 1})
	seed(l, h0-1, map[string]int{StreamTsunami: 2})
	seed(l, h0, map[string]int{StreamForecast: 4, StreamForecast + ":retry": 2})
	u := l.Usage(ctx)
	if u.Total != 10 || u.Limit != 777 || u.WindowHours != 24 {
		t.Fatalf("Usage = %+v", u)
	}
	want := map[string]int{"warning": 3, "earthquake": 1, "tsunami": 2, "cold": 0, "forecast": 4}
	if len(u.ByStream) != 5 {
		t.Fatalf("ByStream must carry all five keys: %v", u.ByStream)
	}
	for k, v := range want {
		if u.ByStream[k] != v {
			t.Fatalf("ByStream[%s] = %d, want %d", k, u.ByStream[k], v)
		}
	}
	// 同一個數：Usage.Total、Gate 的 used_0、警示用的 total() 在同一時刻相同.
	redisOK := false
	l.mu.Lock()
	sn := l.snapshotLocked(ctx, h0, &redisOK)
	gateUsed0 := sn.usedSince(h0 - windowHours)
	l.mu.Unlock()
	if gateUsed0 != u.Total || sn.total() != u.Total {
		t.Fatalf("used_0=%d total()=%d Usage.Total=%d must agree", gateUsed0, sn.total(), u.Total)
	}
	// 時鐘前進一小時：最舊那格掉出視窗.
	clk.Advance(time.Hour)
	if got := l.Usage(ctx).Total; got != 6 {
		t.Fatalf("after +1h Total = %d, want 6", got)
	}
}

// F30-33：Redis 無回應 → 約 1.5 秒放棄改記憶體；下限 1 秒釘住時限沒退回 500 毫秒.
// 慢但會回（兩段都送出）的情境由 TestLedgerReserveSharesOneRedisBudget 守.
func TestLedgerRedisOpTimeout(t *testing.T) {
	logs := captureLogs(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { // 接受連線但永不回應.
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), MaxRetries: -1, ReadTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := newTestClock(t)
	l := NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now)
	start := time.Now()
	if err := l.reserve(context.Background(), CallSpec{Stream: StreamEarthquake, Exempt: true}, 1); err != nil {
		t.Fatalf("reserve must not fail on Redis trouble: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second || el < time.Second {
		t.Fatalf("reserve took %v, want about 1.5s (between 1s and 2s)", el)
	}
	u := l.Usage(context.Background())
	if u.ByStream[StreamEarthquake] != 1 {
		t.Fatalf("the memory-recorded call must show in Usage: %+v", u)
	}
	if out := logs.String(); strings.Contains(out, "127.0.0.1") {
		t.Fatalf("warn leaks address: %s", out)
	}
}

// slowPipelineHook 讓每個 pipeline 先等 delay 才送出：模擬 Redis 慢但會回.
type slowPipelineHook struct{ delay time.Duration }

func (slowPipelineHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (slowPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h slowPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		// 新連線的初始化（CLIENT SETINFO / HELLO）也走 pipeline，而且包在第一個真正的 pipeline 裡面；
		// 不跳過就會把讀那段拖成兩倍、直接逾時，寫那段根本不送，測不到「兩段都慢但都會回」.
		if n := cmds[0].Name(); n != "client" && n != "hello" {
			time.Sleep(h.delay)
		}
		return next(ctx, cmds)
	}
}

// SEC35-01：Redis 慢但會回（讀、寫兩段各慢 1.2 秒）→ 整次 reserve 共用一個 1.5 秒期限，
// 抓取被拖不超過決策 c339b5b0 的 2 秒；寫入那段逾時就退記憶體，這次呼叫照樣計得到.
func TestLedgerReserveSharesOneRedisBudget(t *testing.T) {
	captureLogs(t)
	_, rdb := newRedis(t)
	rdb.AddHook(slowPipelineHook{delay: 1200 * time.Millisecond})
	clk := newTestClock(t)
	l := NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now)
	start := time.Now()
	if err := l.reserve(context.Background(), CallSpec{Stream: StreamEarthquake, Exempt: true}, 1); err != nil {
		t.Fatalf("reserve must not fail on a slow Redis: %v", err)
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Fatalf("reserve took %v with two slow Redis pipelines, want under 2s (one shared 1.5s budget)", el)
	}
	// 放棄等待的寫入事後仍可能寫進 Redis（redisOp 註解的已知限制），所以只要求至少計到一次.
	if u := l.Usage(context.Background()); u.ByStream[StreamEarthquake] < 1 {
		t.Fatalf("the call must be counted (Redis or memory): %+v", u)
	}
}

// slowReplyHook 讓 Redis 先執行命令、回覆才延遲 delay：模擬「寫入已生效但回得慢」.
type slowReplyHook struct{ delay time.Duration }

func (slowReplyHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (slowReplyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h slowReplyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if n := cmds[0].Name(); n != "client" && n != "hello" { // 新連線的初始化不延遲，理由同 slowPipelineHook.
			time.Sleep(h.delay)
		}
		return err
	}
}

// SEC35-03：Redis 先生效、每段回覆慢 0.9 秒 → 讀那段拿到結果，寫那段只剩約 0.6 秒、被判逾時退記憶體，
// 同一筆在 Redis 與記憶體各計 1 次. 這是刻意的保守方向：多計只讓非免擋呼叫提早被擋，少計會讓實際用量超過額度.
// 所以斷言 == 2 —— 把「寫入逾時也記記憶體」拿掉，這支會紅.
func TestLedgerReserveSlowReplyOverCounts(t *testing.T) {
	captureLogs(t)
	mr, rdb := newRedis(t)
	rdb.AddHook(slowReplyHook{delay: 900 * time.Millisecond})
	clk := newTestClock(t)
	l := NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now)
	start := time.Now()
	if err := l.reserve(context.Background(), CallSpec{Stream: StreamEarthquake, Exempt: true}, 1); err != nil {
		t.Fatalf("reserve must not fail on a slow Redis: %v", err)
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Fatalf("reserve took %v, want under 2s", el)
	}
	h0 := hourOf(clk)
	if got := mr.HGet(usageKey(h0), StreamEarthquake); got != "1" {
		t.Fatalf("the write reached Redis before its reply timed out: redis field = %q, want \"1\"", got)
	}
	l.mu.Lock()
	memN := l.mem[h0][StreamEarthquake]
	l.mu.Unlock()
	if memN != 1 {
		t.Fatalf("a timed-out write must also be counted in memory (count more, never less): mem = %d, want 1", memN)
	}
	if u := l.Usage(context.Background()); u.ByStream[StreamEarthquake] != 2 {
		t.Fatalf("Usage must show the conservative double count: %+v", u)
	}
}

// F30-33：每次寫入都重設 TTL（含 TTL 被清掉後再寫入）.
func TestLedgerSetsExpireOnEveryWrite(t *testing.T) {
	captureLogs(t)
	mr, rdb := newRedis(t)
	clk := newTestClock(t)
	l := NewLedger(rdb, testSettings(nil), taipeiLoc(t), clk.Now)
	key := "cwa:usage:" + strconv.FormatInt(hourOf(clk), 10)
	spec := CallSpec{Stream: StreamWarning, Exempt: true}
	if err := l.reserve(context.Background(), spec, 1); err != nil {
		t.Fatal(err)
	}
	if got := mr.TTL(key); got != 26*time.Hour {
		t.Fatalf("TTL after first write = %v, want 26h", got)
	}
	mr.SetTTL(key, 0) // 模擬 TTL 被清掉.
	if got := mr.TTL(key); got != 0 {
		t.Fatalf("setup: TTL still %v", got)
	}
	if err := l.reserve(context.Background(), spec, 1); err != nil {
		t.Fatal(err)
	}
	if got := mr.TTL(key); got != 26*time.Hour {
		t.Fatalf("TTL after second write = %v, want 26h again", got)
	}
}

// F30-33 / V8：用量警示用 floor(limit × warn_ratio)，同一小時格至多一行，log 不含位址.
func TestUsageWarning(t *testing.T) {
	ctx := context.Background()
	spec := CallSpec{Stream: StreamEarthquake, Exempt: true, Dataset: "E-A0015-001"}
	t.Run("default 800", func(t *testing.T) {
		logs := captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		seed(l, hourOf(clk), map[string]int{StreamWarning: 798})
		_ = l.reserve(ctx, spec, 1) // 799.
		if strings.Contains(logs.String(), "cwa quota usage high") {
			t.Fatalf("warned at 799: %s", logs)
		}
		_ = l.reserve(ctx, spec, 1) // 800.
		if out := logs.String(); strings.Count(out, `msg="cwa quota usage high" used=800 limit=1000`) != 1 {
			t.Fatalf("want exactly one warn at 800: %s", out)
		}
		_ = l.reserve(ctx, spec, 1)
		if strings.Count(logs.String(), "cwa quota usage high") != 1 {
			t.Fatalf("same hour must not warn twice: %s", logs)
		}
		clk.Advance(time.Hour)
		_ = l.reserve(ctx, spec, 1)
		if strings.Count(logs.String(), "cwa quota usage high") != 2 {
			t.Fatalf("a new hour cell may warn again: %s", logs)
		}
		for _, bad := range []string{"127.0.0.1", "http://", "?format=", "opendata.cwa.gov.tw", "SECRET"} {
			if strings.Contains(logs.String(), bad) {
				t.Fatalf("warn leaks %q", bad)
			}
		}
	})
	t.Run("warn_ratio 0.5 is honored", func(t *testing.T) {
		logs := captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(func(c *config.Config) { c.CWA.WarnRatio = 0.5 }))
		seed(l, hourOf(clk), map[string]int{StreamWarning: 499})
		_ = l.reserve(ctx, spec, 1)
		if !strings.Contains(logs.String(), "used=500 limit=1000") {
			t.Fatalf("threshold must follow warn_ratio: %s", logs)
		}
	})
}
