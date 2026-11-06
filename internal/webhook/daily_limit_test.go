package webhook

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testClock 可前進的假時鐘（F24-32、F24-33 的測試共用）.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(start time.Time) *testClock { return &testClock{t: start} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newDailyHandler 建立沒有 Redis、帶假時鐘的 handler.
func newDailyHandler(clk *testClock) *WebhookHandler {
	h := NewWebhookHandler(nil, nil, nil, nil, nil)
	h.daily = localcache.NewCounterWithClock("daily_limit", dailyLimitWarnAbove, clk.now)
	h.clock = clk.now
	return h
}

// unreachableRedis 連不上的 Redis 用戶端.
func unreachableRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// warnRecorder 記下所有 Warn 以上的 log 訊息.
type warnRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *warnRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *warnRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *warnRecorder) WithGroup(string) slog.Handler            { return r }

//nolint:gocritic // slog.Handler 介面固定以值傳遞 slog.Record，無法改成指標.
func (r *warnRecorder) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level < slog.LevelWarn {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Message)
	return nil
}

func (r *warnRecorder) count(msg string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// TestWithinDailyLimitLocalFallback [F24-32] 沒有 Redis 時第 31 次 photo 被擋.
func TestWithinDailyLimitLocalFallback(t *testing.T) {
	tests := []struct {
		name  string
		limit int64
	}{
		{"photo 30", 30},
		{"limit 1", 1},
		{"limit 60", 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDailyHandler(newTestClock(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)))
			ctx := context.Background()
			for i := int64(1); i <= tt.limit; i++ {
				if !h.withinDailyLimit(ctx, "photo", "U1", tt.limit) {
					t.Fatalf("call %d rejected, want allowed (limit %d)", i, tt.limit)
				}
			}
			if h.withinDailyLimit(ctx, "photo", "U1", tt.limit) {
				t.Errorf("call %d allowed, want rejected", tt.limit+1)
			}
		})
	}
}

// TestWithinDailyLimitPerUserAndKind [F24-32] 計數依使用者與 kind 各自獨立.
func TestWithinDailyLimitPerUserAndKind(t *testing.T) {
	h := newDailyHandler(newTestClock(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)))
	ctx := context.Background()
	if !h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("U1 photo first call rejected")
	}
	if h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("U1 photo second call allowed, want rejected")
	}
	tests := []struct {
		name         string
		kind, userID string
	}{
		{"other user same kind", "photo", "U2"},
		{"same user qa:count", "qa:count", "U1"},
		{"same user mealtext", "mealtext", "U1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !h.withinDailyLimit(ctx, tt.kind, tt.userID, 1) {
				t.Errorf("first call for %s/%s rejected, counts must be independent", tt.kind, tt.userID)
			}
		})
	}
}

// TestWithinDailyLimitRedisDownFallsBackLocal [F24-32] Redis 連不上（Incr 出錯）時改用本地計數.
func TestWithinDailyLimitRedisDownFallsBackLocal(t *testing.T) {
	h := newDailyHandler(newTestClock(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)))
	h.limiter = unreachableRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("first call rejected, want allowed")
	}
	if h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Error("second call allowed with Redis down, want rejected by local count")
	}
}

// TestWithinDailyLimitResetsNextTaipeiDay [F24-32] key 以台北日期換日.
func TestWithinDailyLimitResetsNextTaipeiDay(t *testing.T) {
	// 2026-11-04 15:59 UTC = 台北 23:59
	clk := newTestClock(time.Date(2026, 11, 4, 15, 59, 0, 0, time.UTC))
	h := newDailyHandler(clk)
	ctx := context.Background()
	if !h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("first call rejected")
	}
	if h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("second call same Taipei day allowed, want rejected")
	}
	clk.advance(2 * time.Minute) // 台北 11/05 00:01
	if !h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Error("first call on next Taipei day rejected, want count restarted")
	}
}

// TestWithinDailyLimitKeepsAllUsers [F24-32] 大量使用者各用 1 次後，第 2 次全被擋（計數不被淘汰）.
func TestWithinDailyLimitKeepsAllUsers(t *testing.T) {
	h := newDailyHandler(newTestClock(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)))
	ctx := context.Background()
	const users = 2000
	for i := 0; i < users; i++ {
		if !h.withinDailyLimit(ctx, "photo", fmt.Sprintf("U%d", i), 1) {
			t.Fatalf("user %d first call rejected", i)
		}
	}
	allowed := 0
	for i := 0; i < users; i++ {
		if h.withinDailyLimit(ctx, "photo", fmt.Sprintf("U%d", i), 1) {
			allowed++
		}
	}
	if allowed != 0 {
		t.Errorf("%d of %d users got a second call, want 0 (counts evicted)", allowed, users)
	}
}

// TestWithinDailyLimitCanceledContextCountsLocally [F24-32] ctx 已取消也走本地計數，且不發 Warn.
func TestWithinDailyLimitCanceledContextCountsLocally(t *testing.T) {
	rec := &warnRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newDailyHandler(newTestClock(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)))
	h.limiter = unreachableRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Fatal("first call rejected, want allowed")
	}
	if h.withinDailyLimit(ctx, "photo", "U1", 1) {
		t.Error("second call allowed with canceled ctx, want rejected by local count")
	}
	if n := rec.count("daily limiter unavailable"); n != 0 {
		t.Errorf("got %d 'daily limiter unavailable' warnings, want 0 for canceled ctx", n)
	}
}
