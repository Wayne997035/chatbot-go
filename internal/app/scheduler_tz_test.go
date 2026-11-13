package app

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// alrtLogBuf 可並行寫入的 log 緩衝（排程的背景 goroutine 也會寫）.
type alrtLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *alrtLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *alrtLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// alrtDisabledLine 只由 startSchedulers 的 a.taipei == nil 判斷印出（build 另印一行不同字樣的 Error）.
const alrtDisabledLine = "CWA schedulers disabled: taipei timezone unavailable"

// [F30-45] 時區載入失敗：預報與警報排程都不啟動（1 秒內氣象署收到 0 個請求），build 與 startSchedulers 不 panic、不回錯，
// log 恰一行說明；對照組：時區正常時 1 秒內至少收到 1 個請求（證明探針叫得起來）.
func TestSchedulersDisabledWhenTaipeiUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		taipeiFails  bool
		wantRequests func(n int32) bool
		wantLines    int
	}{
		{"taipei unavailable", true, func(n int32) bool { return n == 0 }, 1},
		{"taipei available (control)", false, func(n int32) bool { return n >= 1 }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)

			logs := &alrtLogBuf{}
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })

			if tc.taipeiFails {
				prev := loadTaipei
				loadTaipei = func() (*time.Location, error) { return nil, errors.New("tzdata missing") }
				t.Cleanup(func() { loadTaipei = prev })
			}

			cfg := &config.Config{
				Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "test-key", Cron: "0 0 1 1 *"},
			}
			a, err := build(cfg, unreachableDB(t), nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			a.startSchedulers(cfg)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = a.alertScheduler.Stop(ctx)
				_ = a.weatherScheduler.Stop(ctx)
			})

			time.Sleep(time.Second)
			if got := requests.Load(); !tc.wantRequests(got) {
				t.Errorf("CWA requests within 1s = %d", got)
			}
			if got := strings.Count(logs.String(), alrtDisabledLine); got != tc.wantLines {
				t.Errorf("%q appears %d times, want %d: %s", alrtDisabledLine, got, tc.wantLines, logs.String())
			}
		})
	}
}
