package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

var (
	firstCall = CallSpec{Stream: StreamEarthquake, Exempt: true}
	lowCall   = CallSpec{Stream: StreamForecast}
)

// F30-35：額度用完回應 → 暫停低優先呼叫；只有「第一次呼叫」回 2xx 解除.
func TestExhaustedStatusPausesLowPriority(t *testing.T) {
	ctx := context.Background()
	t.Run("429 pauses, is not retried, first calls still go out, only exempt first 2xx resumes", func(t *testing.T) {
		captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		status := 429
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			okJSON(w)
		})
		c := newTestClient(ts, l, testSettings(nil), nil)
		var out decoded
		if err := c.GetJSON(ctx, firstCall, "x", nil, &out); ClassOf(err) != "status_429" || ts.count() != 1 {
			t.Fatalf("429 must not be retried: err=%v reqs=%d", err, ts.count())
		}
		if !isPaused(l) {
			t.Fatal("429 must pause")
		}
		for _, spec := range []CallSpec{{Stream: StreamCold}, {Stream: StreamForecast}, {Stream: StreamWarning}} {
			if err := l.reserve(ctx, spec, 1); !errors.Is(err, ErrQuotaReserved) {
				t.Fatalf("paused low-priority call %s must be denied even at low usage: %v", spec.Stream, err)
			}
		}
		if err := l.reserve(ctx, firstCall, 1); err != nil {
			t.Fatalf("first calls go out while paused: %v", err)
		}
		status = 200
		if err := c.GetJSON(ctx, lowCall, "x", nil, &out); err == nil {
			t.Fatal("non-exempt call must be denied while paused")
		}
		l.noteResponse(lowCall, 1, 200)
		if !isPaused(l) {
			t.Fatal("a 200 from a non-exempt call must not resume")
		}
		if err := c.GetJSON(ctx, firstCall, "x", nil, &out); err != nil {
			t.Fatal(err)
		}
		if isPaused(l) {
			t.Fatal("exempt first-attempt 200 must resume")
		}
	})
	t.Run("only exempt first 2xx resumes; other outcomes keep the pause", func(t *testing.T) {
		for _, code := range []int{401, 404, 500, 503, 302} {
			captureLogs(t)
			l := newMemLedger(t, newTestClock(t), testSettings(nil))
			l.noteResponse(firstCall, 1, 429)
			l.noteResponse(firstCall, 1, code)
			if !isPaused(l) {
				t.Fatalf("status %d must not change the pause", code)
			}
		}
		captureLogs(t)
		l := newMemLedger(t, newTestClock(t), testSettings(nil))
		l.noteResponse(firstCall, 1, 429)
		l.noteResponse(firstCall, 2, 200) // 重試不算「第一次」.
		if !isPaused(l) {
			t.Fatal("a retry attempt must not resume")
		}
		// 暫停中第一次呼叫連線失敗：沒有回應 → 狀態不變.
		dead := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
		cl := newTestClient(dead, l, testSettings(nil), nil)
		dead.Close()
		var out decoded
		_ = cl.GetJSON(ctx, firstCall, "x", nil, &out)
		if !isPaused(l) {
			t.Fatal("network error must not resume")
		}
	})
	t.Run("another 429 while paused resets the 24h timer; auto resume after 24h", func(t *testing.T) {
		captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		l.noteResponse(firstCall, 1, 429)
		clk.Advance(23 * time.Hour)
		l.noteResponse(firstCall, 1, 429) // 重設.
		clk.Advance(2 * time.Hour)        // 距第一次 25 小時，距重設 2 小時.
		_ = l.reserve(ctx, firstCall, 1)  // 觸發過期檢查.
		if !isPaused(l) {
			t.Fatal("timer must have been reset by the second 429")
		}
		clk.Advance(22 * time.Hour) // 距重設恰 24 小時.
		_ = l.reserve(ctx, firstCall, 1)
		if isPaused(l) {
			t.Fatal("pause must auto-release after 24h")
		}
	})
	t.Run("list is configurable", func(t *testing.T) {
		captureLogs(t)
		s := testSettings(func(c *config.Config) { c.CWA.ExhaustedStatusCodes = "403" })
		l := newMemLedger(t, newTestClock(t), s)
		l.noteResponse(firstCall, 1, 429)
		if isPaused(l) {
			t.Fatal("429 is not in the list")
		}
		l.noteResponse(firstCall, 1, 403)
		if !isPaused(l) {
			t.Fatal("403 must pause")
		}
	})
}

// F30-35：清單內狀態碼優先於 5xx 重試.
func TestExhaustedStatusTakesPrecedenceOverRetry(t *testing.T) {
	always503 := func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }
	t.Run("503 in list: one request and paused", func(t *testing.T) {
		captureLogs(t)
		s := testSettings(func(c *config.Config) { c.CWA.ExhaustedStatusCodes = "429,503" })
		l := newMemLedger(t, newTestClock(t), s)
		ts := newTestServer(t, always503)
		var out decoded
		err := newTestClient(ts, l, s, nil).GetJSON(context.Background(), firstCall, "x", nil, &out)
		if ts.count() != 1 || ClassOf(err) != "status_503" || !isPaused(l) {
			t.Fatalf("reqs=%d err=%v paused=%v", ts.count(), err, isPaused(l))
		}
	})
	t.Run("503 not in list: retried, not paused", func(t *testing.T) {
		captureLogs(t)
		s := testSettings(nil)
		l := newMemLedger(t, newTestClock(t), s)
		ts := newTestServer(t, always503)
		var out decoded
		_ = newTestClient(ts, l, s, nil).GetJSON(context.Background(), firstCall, "x", nil, &out)
		if ts.count() != 2 || isPaused(l) {
			t.Fatalf("reqs=%d paused=%v, want 2 and not paused", ts.count(), isPaused(l))
		}
	})
}
