package alert

import (
	"context"
	"fmt"
	"log/slog"
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
