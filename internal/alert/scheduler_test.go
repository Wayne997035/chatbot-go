package alert

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// captureHandler 記錄所有等級的 log record.
type logRec struct {
	Level   slog.Level
	Message string
}

type captureHandler struct {
	mu   sync.Mutex
	recs []logRec
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler 介面規定以值傳遞 Record
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, logRec{Level: r.Level, Message: r.Message})
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) find(msg string) (slog.Level, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r.Level, true
		}
	}
	return 0, false
}

func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return h
}

// [F24-08] 正常完成的檢查不產生 Info 以上的 log，Debug 有紀錄.
func TestRunCheckSuccessLogsDebugOnly(t *testing.T) {
	feed := &cwaFeed{quakeTime: "2026-11-02 17:30:00"} // 太舊，不推播，避免 checker 自己的 Info
	c := newTestChecker(t, feed, newNotifier())
	h := captureLogs(t)

	(&Scheduler{checker: c, cfg: c.cfg}).runCheck(context.Background())

	h.mu.Lock()
	for _, r := range h.recs {
		if r.Level >= slog.LevelInfo {
			t.Errorf("unexpected %v record on success: %q", r.Level, r.Message)
		}
	}
	h.mu.Unlock()
	if lvl, ok := h.find("alert check completed"); !ok || lvl != slog.LevelDebug {
		t.Errorf("completed record: found=%v level=%v, want Debug", ok, lvl)
	}
}

// [F24-08] 檢查失敗仍印 Error，且不印 completed.
func TestRunCheckFailureLogsError(t *testing.T) {
	c := newTestChecker(t, &cwaFeed{}, newNotifier())
	c.cfg.Weather.CWABaseURL = "http://127.0.0.1:1" // 連不上
	h := captureLogs(t)

	(&Scheduler{checker: c, cfg: c.cfg}).runCheck(context.Background())

	if lvl, ok := h.find("alert check failed"); !ok || lvl != slog.LevelError {
		t.Errorf("failed record: found=%v level=%v, want Error", ok, lvl)
	}
	if _, ok := h.find("alert check completed"); ok {
		t.Error("completed must not be logged on failure")
	}
}
