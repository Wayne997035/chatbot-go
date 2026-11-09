package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeClock 可前進的假時鐘；起點刻意不是 Unix 0（localcache 的 Warn 限流以「上次時間 == 0」判第一筆）.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// warnCapture 擷取 slog Warn 的 handler.
type warnCapture struct {
	mu    sync.Mutex
	warns []string
}

func (h *warnCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnCapture) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *warnCapture) WithGroup(string) slog.Handler            { return h }

//nolint:gocritic // slog.Handler interface requires slog.Record by value.
func (h *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.mu.Lock()
	h.warns = append(h.warns, line)
	h.mu.Unlock()
	return nil
}

func (h *warnCapture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.warns)
}

func captureWarns(t *testing.T) *warnCapture {
	t.Helper()
	h := &warnCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return h
}

// [F24-51] TestSentLogClaimIsOnce 守住對應行為.
func TestSentLogClaimIsOnce(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	if !s.claim(ctx, "k") {
		t.Fatal("first claim must be true")
	}
	if s.claim(ctx, "k") {
		t.Fatal("second claim must be false")
	}
}

// [F24-51] TestSentLogClaimIsAtomicUnderConcurrency 守住對應行為.
func TestSentLogClaimIsAtomicUnderConcurrency(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	var wins atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.claim(context.Background(), "same") {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("claims won = %d, want exactly 1", got)
	}
}

// [F24-51] TestSentLogClaimDistinctKeys 守住對應行為.
func TestSentLogClaimDistinctKeys(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "a:b", "ab"} {
		if !s.claim(ctx, k) {
			t.Errorf("claim(%q) = false, want true", k)
		}
	}
}

// [F24-51] Redis 不可用：不 panic、不阻塞、退回本地語意.
func TestSentLogFallsBackToMemoryWhenRedisFails(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })
	s := newSentLogWithClock(down, newFakeClock().now)
	ctx := context.Background()

	begin := time.Now()
	if !s.claim(ctx, "k") {
		t.Fatal("a Redis failure must not reject the first claim")
	}
	if s.claim(ctx, "k") {
		t.Fatal("second claim must be false from memory")
	}
	s.failed(ctx, "k")
	if !s.claim(ctx, "k") {
		t.Fatal("after failed below the limit the claim is released")
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("fallback took %v, want < 2s", d)
	}
}

// [F24-51] 寫入 10 倍 warnAbove 筆後，每一個 key 都仍被記得（被拒收的是新 key，所以逐 key 重查），且有 Warn.
func TestSentLogNeverEvictsBeyondWarnAbove(t *testing.T) {
	h := captureWarns(t)
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	n := 10 * sentLogWarnAbove

	if !s.claim(ctx, "first") {
		t.Fatal("first claim must be true")
	}
	for i := range n {
		if !s.claim(ctx, fmt.Sprintf("k%d", i)) {
			t.Fatalf("claim k%d = false on first write", i)
		}
	}
	if s.claim(ctx, "first") {
		t.Error("first was evicted")
	}
	for i := range n {
		if s.claim(ctx, fmt.Sprintf("k%d", i)) {
			t.Fatalf("k%d was evicted or rejected: claim returned true again", i)
		}
	}
	if h.count() < 1 {
		t.Error("want >=1 Warn once above sentLogWarnAbove, got none")
	}
}

// [F24-52] TestSentLogFailedReleasesBelowLimit 守住對應行為.
func TestSentLogFailedReleasesBelowLimit(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	if !s.claim(ctx, "k") {
		t.Fatal("claim")
	}
	s.failed(ctx, "k")
	if !s.claim(ctx, "k") {
		t.Fatal("claim after 1 failure must be true (released)")
	}
}

// sentFlakyHook go-redis Hook：down 為 true 時指令回 error，否則不呼叫 next 直接當作成功（不連網路）.
type sentFlakyHook struct{ down atomic.Bool }

func (h *sentFlakyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *sentFlakyHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if h.down.Load() {
			err := errors.New("redis down")
			cmd.SetErr(err)
			return err
		}
		if ic, ok := cmd.(*redis.IntCmd); ok {
			ic.SetVal(1) // Incr：第 1 次失敗，tries < maxPushTries
		}
		return nil
	}
}

func (h *sentFlakyHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, _ []redis.Cmder) error { return nil }
}

// [F24-71] Redis 壞 -> 好 -> 壞：故障期間的本地佔位，在 Redis 成功釋放（tries < maxPushTries）後要清掉，
// 第二次壞時 claim 才能重試；否則讀本地回 false，該重試的警報不再重試.
func TestSentLogFailedClearsLocalClaimAfterRedisRecovers(t *testing.T) {
	ctx := context.Background()
	hook := &sentFlakyHook{}
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	s := newSentLogWithClock(c, newFakeClock().now)

	hook.down.Store(true)
	if !s.claim(ctx, "k") { // 壞：佔位寫進本地
		t.Fatal("first claim must succeed from memory")
	}
	if s.claim(ctx, "k") {
		t.Fatal("second claim must be false from memory")
	}
	hook.down.Store(false)
	s.failed(ctx, "k") // 好：Redis Incr 成功、tries=1 < maxPushTries，釋放佔位
	hook.down.Store(true)
	if !s.claim(ctx, "k") {
		t.Fatal("stale local claim must be cleared once Redis released the placeholder")
	}
}

// [F24-52] TestSentLogFailedKeepsClaimAtLimit 守住對應行為.
func TestSentLogFailedKeepsClaimAtLimit(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	for i := 1; i <= maxPushTries; i++ {
		if !s.claim(ctx, "k") {
			t.Fatalf("claim before failure %d must be true", i)
		}
		s.failed(ctx, "k")
	}
	if s.claim(ctx, "k") {
		t.Fatal("after maxPushTries failures the claim must stay held")
	}
}

// [F24-52] TestSentLogTriesAreIndependentPerKey 守住對應行為.
func TestSentLogTriesAreIndependentPerKey(t *testing.T) {
	s := newSentLogWithClock(nil, newFakeClock().now)
	ctx := context.Background()
	for range maxPushTries - 1 {
		s.claim(ctx, "A")
		s.failed(ctx, "A")
	}
	s.claim(ctx, "B")
	s.failed(ctx, "B")

	if !s.claim(ctx, "B") {
		t.Fatal("B failed once, must still be retryable")
	}
	if !s.claim(ctx, "A") {
		t.Fatal("A failed twice, must still be retryable")
	}
	s.failed(ctx, "A")
	if s.claim(ctx, "A") {
		t.Fatal("A failed 3 times, must be locked")
	}
	if s.claim(ctx, "B") {
		t.Fatal("B holds its claim after the successful re-claim")
	}
}

// [F24-52] 計數不因數量或時間流逝歸零：中間塞滿 10 倍 warnAbove 個 key 並前進 24 小時（< sentTTL）.
func TestSentLogTriesNeverEvictedBeyondWarnAbove(t *testing.T) {
	clk := newFakeClock()
	s := newSentLogWithClock(nil, clk.now)
	ctx := context.Background()

	for range 2 {
		if !s.claim(ctx, "first") {
			t.Fatal("claim(first) must be true before failed")
		}
		s.failed(ctx, "first")
	}
	if !s.claim(ctx, "first") {
		t.Fatal("claim(first) before the 3rd failure must be true")
	}
	for i := range 10 * sentLogWarnAbove {
		s.failed(ctx, fmt.Sprintf("k%d", i))
	}
	clk.advance(24 * time.Hour)
	s.failed(ctx, "first")
	if s.claim(ctx, "first") {
		t.Fatal("3rd failure must keep the claim: the count was lost")
	}
}

// scanPage [F241-11] 假 Redis 預先排好的一頁 SCAN 結果.
type scanPage struct {
	keys []string
	next uint64
}

// [F241-11] erasureFakeRedis 以 Hook 實作的假 Redis；指令形狀取自真 go-redis v9 client 的實測
// （SetNX 送 set ... ex 秒數 nx；Scan 送 [scan 游標 match 樣式 count N]）.
// 未處理的指令一律 t.Errorf 並回傳 error，不得 return nil.
type erasureFakeRedis struct {
	t    *testing.T
	mu   sync.Mutex
	data map[string]string
	// failAll 非 nil 時所有指令都 SetErr 並回傳同一個 error（模擬 Redis 整個掛掉，屬刻意注入，不觸發 t.Errorf）.
	failAll error
	// pages 樣式 -> 游標 -> 預先排好的 SCAN 頁（回傳的鍵與下一個游標；最後一頁的下一個游標為 0）.
	pages map[string]map[uint64]scanPage
	// delKeyCounts 每次 DEL 帶了幾把鍵（設計要求逐鍵）.
	delKeyCounts []int
}

func newErasureFakeRedis(t *testing.T) *erasureFakeRedis {
	t.Helper()
	return &erasureFakeRedis{t: t, data: map[string]string{}}
}

func (f *erasureFakeRedis) client() *redis.Client {
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rc.AddHook(f)
	return rc
}

func (f *erasureFakeRedis) put(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.data[k] = "1"
	}
}

func (f *erasureFakeRedis) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

func (f *erasureFakeRedis) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fakeArgStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func (f *erasureFakeRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial blocked") }
}

func (f *erasureFakeRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *erasureFakeRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failAll != nil {
			cmd.SetErr(f.failAll)
			return f.failAll
		}
		args := cmd.Args()
		name := strings.ToLower(fakeArgStr(args[0]))
		switch c := cmd.(type) {
		case *redis.BoolCmd:
			switch name {
			case "set": // SetNX
				k := fakeArgStr(args[1])
				if _, ok := f.data[k]; ok {
					c.SetVal(false)
					return nil
				}
				f.data[k] = fakeArgStr(args[2])
				c.SetVal(true)
				return nil
			case "expire":
				c.SetVal(true)
				return nil
			}
		case *redis.IntCmd:
			switch name {
			case "del":
				f.delKeyCounts = append(f.delKeyCounts, len(args)-1)
				n := 0
				for _, a := range args[1:] {
					if _, ok := f.data[fakeArgStr(a)]; ok {
						delete(f.data, fakeArgStr(a))
						n++
					}
				}
				c.SetVal(int64(n))
				return nil
			case "incr":
				k := fakeArgStr(args[1])
				cur, _ := strconv.Atoi(f.data[k])
				cur++
				f.data[k] = strconv.Itoa(cur)
				c.SetVal(int64(cur))
				return nil
			}
		case *redis.ScanCmd:
			cursor, _ := args[1].(uint64)
			pat := fakeArgStr(args[3])
			if pages, ok := f.pages[pat]; ok {
				page, found := pages[cursor]
				if !found {
					err := fmt.Errorf("scripted scan: no page for cursor %d", cursor)
					f.t.Errorf("%v", err)
					c.SetErr(err)
					return err
				}
				c.SetVal(page.keys, page.next)
				return nil
			}
			var keys []string
			for k := range f.data {
				if ok, _ := path.Match(pat, k); ok {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			c.SetVal(keys, 0)
			return nil
		}
		err := fmt.Errorf("unsupported command %s %T", name, cmd)
		f.t.Errorf("%v", err)
		cmd.SetErr(err)
		return err
	}
}

func assertAllSingleKeyDel(t *testing.T, f *erasureFakeRedis) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delKeyCounts) == 0 {
		t.Fatal("no DEL was issued")
	}
	for i, n := range f.delKeyCounts {
		if n != 1 {
			t.Errorf("DEL call %d carried %d keys, want 1 (per-key delete)", i, n)
		}
	}
}

// [F241-11] 本地備援：Redis 為 nil 時佔位與重試計數都要清，別人（含結尾相似的 XU1）不受影響.
func TestSentLogForgetRecipientLocal(t *testing.T) {
	ctx := context.Background()
	s := newSentLogWithClock(nil, newFakeClock().now)
	u1a, u1b := "earthquake:5:U1", "weather:台北市:大雨特報:U1"
	u2, xu1 := "earthquake:5:U2", "earthquake:5:XU1"
	for _, k := range []string{u1a, u1b, u2, xu1} {
		if !s.claim(ctx, k) {
			t.Fatalf("control: first claim of %q must be true", k)
		}
	}
	s.failed(ctx, u1a) // 釋放佔位並留下本地計數 1

	if err := s.forgetRecipient(ctx, "U1"); err != nil {
		t.Fatalf("forgetRecipient = %v, want nil", err)
	}
	if !s.claim(ctx, u1b) {
		t.Error("U1 weather claim must be released")
	}
	if s.claim(ctx, u2) {
		t.Error("U2 claim must survive")
	}
	if s.claim(ctx, xu1) {
		t.Error("XU1 claim must survive (suffix match includes the colon)")
	}
	// 計數歸零：兩輪 claim→failed 都要能 claim，第 3 次仍 true，failed 後才達上限保留佔位.
	for round := 1; round <= 2; round++ {
		if !s.claim(ctx, u1a) {
			t.Fatalf("round %d: claim must be true (tries reset)", round)
		}
		s.failed(ctx, u1a)
	}
	if !s.claim(ctx, u1a) {
		t.Fatal("third claim must be true")
	}
	s.failed(ctx, u1a)
	if s.claim(ctx, u1a) {
		t.Error("claim after reaching maxPushTries must be false")
	}
}

// [F241-11] Redis 兩種樣式（佔位與 :tries）都清，只刪 U1，逐鍵 DEL.
func TestSentLogForgetRecipientRedis(t *testing.T) {
	f := newErasureFakeRedis(t)
	f.put("alert:sent:earthquake:5:U1", "alert:sent:earthquake:5:U1:tries",
		"alert:sent:earthquake:5:U2", "alert:sent:earthquake:5:U2:tries", "alert:sent:earthquake:5:XU1")
	s := newSentLogWithClock(f.client(), newFakeClock().now)

	if err := s.forgetRecipient(context.Background(), "U1"); err != nil {
		t.Fatalf("forgetRecipient = %v, want nil", err)
	}
	for _, k := range []string{"alert:sent:earthquake:5:U1", "alert:sent:earthquake:5:U1:tries"} {
		if f.has(k) {
			t.Errorf("%q must be deleted", k)
		}
	}
	for _, k := range []string{"alert:sent:earthquake:5:U2", "alert:sent:earthquake:5:U2:tries", "alert:sent:earthquake:5:XU1"} {
		if !f.has(k) {
			t.Errorf("%q must survive", k)
		}
	}
	assertAllSingleKeyDel(t, f)
}

// [F241-11] userID 含 glob 字元時必須跳脫，只刪自己的鍵.
func TestSentLogForgetRecipientEscapesGlob(t *testing.T) {
	for _, uid := range []string{"U*", "U?", "U[A]"} {
		t.Run(uid, func(t *testing.T) {
			f := newErasureFakeRedis(t)
			own := "alert:sent:earthquake:5:" + uid
			other := "alert:sent:earthquake:5:UA"
			f.put(own, own+":tries", other, other+":tries")
			s := newSentLogWithClock(f.client(), newFakeClock().now)

			if err := s.forgetRecipient(context.Background(), uid); err != nil {
				t.Fatalf("forgetRecipient = %v, want nil", err)
			}
			if f.has(own) || f.has(own+":tries") {
				t.Errorf("own keys must be deleted, left: %v", f.keys())
			}
			if !f.has(other) || !f.has(other+":tries") {
				t.Errorf("UA keys must survive, left: %v", f.keys())
			}
		})
	}
}

// [F241-11] Redis 整個掛掉：本地照清，回傳非 nil 錯誤且不含 userID.
func TestSentLogForgetRecipientRedisDownClearsLocal(t *testing.T) {
	ctx := context.Background()
	f := newErasureFakeRedis(t)
	f.failAll = errors.New("redis down")
	s := newSentLogWithClock(f.client(), newFakeClock().now)
	if !s.claim(ctx, "earthquake:5:U1") || !s.claim(ctx, "earthquake:5:U2") {
		t.Fatal("control: claims land in local memory and must be true")
	}
	if s.claim(ctx, "earthquake:5:U1") {
		t.Fatal("control: local claim must be recorded")
	}

	err := s.forgetRecipient(ctx, "U1")
	if err == nil {
		t.Fatal("forgetRecipient must report Redis failure")
	}
	if strings.Contains(err.Error(), "U1") {
		t.Errorf("error must not contain userID: %v", err)
	}
	if !s.claim(ctx, "earthquake:5:U1") {
		t.Error("U1 local claim must be cleared even though Redis failed")
	}
	if s.claim(ctx, "earthquake:5:U2") {
		t.Error("U2 local claim must survive")
	}
}

// [F241-11] SCAN 分頁：游標非 0 要繼續換頁到 0.
func TestSentLogForgetRecipientFollowsCursor(t *testing.T) {
	f := newErasureFakeRedis(t)
	k1, k2 := "alert:sent:earthquake:5:U1", "alert:sent:earthquake:6:U1"
	f.put(k1, k2)
	f.pages = map[string]map[uint64]scanPage{"alert:sent:*:U1": {0: {keys: []string{k1}, next: 7}, 7: {keys: []string{k2}, next: 0}}}
	s := newSentLogWithClock(f.client(), newFakeClock().now)

	if err := s.forgetRecipient(context.Background(), "U1"); err != nil {
		t.Fatalf("forgetRecipient = %v, want nil", err)
	}
	if f.has(k1) || f.has(k2) {
		t.Errorf("both pages must be deleted, left: %v", f.keys())
	}
}

// [F241-11] 空 userID 不得變成 alert:sent:*: 樣式刪到別人.
func TestSentLogForgetRecipientEmptyID(t *testing.T) {
	f := newErasureFakeRedis(t)
	empty, u1 := "alert:sent:earthquake:5:", "alert:sent:earthquake:5:U1"
	f.put(empty, u1)
	s := newSentLogWithClock(f.client(), newFakeClock().now)
	s.sent.Set(empty, struct{}{})
	s.sent.Set(u1, struct{}{})

	if err := s.forgetRecipient(context.Background(), ""); err != nil {
		t.Fatalf("forgetRecipient = %v, want nil", err)
	}
	for _, k := range []string{empty, u1} {
		if !f.has(k) {
			t.Errorf("redis key %q must remain", k)
		}
		if _, ok := s.sent.Get(k); !ok {
			t.Errorf("local key %q must remain", k)
		}
	}
}
