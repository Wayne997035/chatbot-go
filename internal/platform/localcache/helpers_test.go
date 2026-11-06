package localcache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 是可手動前進的時鐘，測試一律用它推進時間，不用 time.Sleep.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// syncExec 讓 OnDeletion 在呼叫端同步執行.
func syncExec(fn func()) { fn() }

// testCfg 是測試用的建構設定：假時鐘 + 同步 Executor.
func testCfg(name string, maxSize int, clk *fakeClock) config {
	return config{name: name, maxSize: maxSize, now: clk.Now, exec: syncExec}
}

// logRecorder 擷取 slog 輸出，測試結束還原預設 logger.
type logRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *logRecorder) WithGroup(string) slog.Handler            { return r }

//nolint:gocritic // slog.Handler 介面固定以值傳遞 slog.Record，無法改成指標.
func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String() + " " + rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	r.mu.Lock()
	r.msgs = append(r.msgs, b.String())
	r.mu.Unlock()
	return nil
}

// warns 回傳含 substr 的 Warn 筆數與內容.
func (r *logRecorder) warns(substr string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		if strings.HasPrefix(m, "WARN") && strings.Contains(m, substr) {
			out = append(out, m)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	old := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })
	return rec
}
