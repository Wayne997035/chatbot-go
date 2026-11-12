package cwa

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// safeBuf 可並行寫入的 buffer（給 slog 捕捉用）.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuf) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// captureLogs 把預設 slog 導到 buffer，測完還原.
func captureLogs(t *testing.T) *safeBuf {
	t.Helper()
	b := &safeBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

// fakeClock 可注入、可前進的時鐘.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func taipeiLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadTaipei(nil)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// testSettings 預設設定，可由 mutate 修改（經 Config 以保證與正式路徑同一份邏輯）.
func testSettings(mutate func(*config.Config)) config.CWASettings {
	c := &config.Config{}
	if mutate != nil {
		mutate(c)
	}
	return c.CWASettings()
}

// newTestClock 台灣時間 2026-11-10 12:02:00（不對齊 5 分鐘，驗證推算不依賴整點）.
func newTestClock(t *testing.T) *fakeClock {
	t.Helper()
	return &fakeClock{t: time.Date(2026, 11, 10, 12, 2, 0, 0, taipeiLoc(t))}
}

func hourOf(c *fakeClock) int64 { return c.Now().Unix() / 3600 }

// seed 直接寫進記憶體記帳（測試用：只有無 Redis 的 Ledger 才讀得到）.
func seed(l *Ledger, hour int64, fields map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cell := l.mem[hour]
	if cell == nil {
		cell = map[string]int{}
		l.mem[hour] = cell
	}
	for f, n := range fields {
		cell[f] += n
	}
}

func isPaused(l *Ledger) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

func slogDefault() *slog.Logger { return slog.Default() }
