package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var warnSpec = CallSpec{Stream: StreamWarning, Exempt: true, Dataset: "W-C0033-001"}

func getOK(c *Client, spec CallSpec) (decoded, error) {
	var out decoded
	err := c.GetJSON(context.Background(), spec, "W-C0033-001", url.Values{"format": {"JSON"}}, &out)
	return out, err
}

// F30-31.
func TestClientRetryMatrix(t *testing.T) {
	s := testSettings(nil)
	tests := []struct {
		name      string
		handler   func(n int, w http.ResponseWriter, r *http.Request)
		maxBody   int64
		wantReqs  int
		wantClass string // 空 = 成功.
	}{
		{"reset then 200", func(n int, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				hijackClose(w)
				return
			}
			okJSON(w)
		}, 0, 2, ""},
		{"503 then 200", func(n int, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			okJSON(w)
		}, 0, 2, ""},
		{"always 503 exactly 2", func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, 0, 2, "status_503"},
		{"always reset exactly 2", func(_ int, w http.ResponseWriter, _ *http.Request) { hijackClose(w) }, 0, 2, "network"},
		{"401 once", func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, 0, 1, "status_401"},
		{"403 once", func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }, 0, 1, "status_403"},
		{"404 once", func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }, 0, 1, "status_404"},
		{"429 once", func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, 0, 1, "status_429"},
		{"bad JSON once", func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }, 0, 1, "decode"},
		{"too large once", func(_ int, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", 64) + `{"ok":true}`))
		}, 32, 1, "too_large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			ts := newTestServer(t, tc.handler)
			c := newTestClient(ts, nil, s, func(o *ClientOptions) { o.MaxBody = tc.maxBody })
			out, err := getOK(c, warnSpec)
			if ts.count() != tc.wantReqs {
				t.Fatalf("requests = %d, want %d (err=%v)", ts.count(), tc.wantReqs, err)
			}
			if tc.wantClass == "" {
				if err != nil || !out.OK {
					t.Fatalf("want success, err=%v out=%+v", err, out)
				}
				return
			}
			var ce *Error
			if !errors.As(err, &ce) || ce.Class != tc.wantClass || ce.Attempts != tc.wantReqs {
				t.Fatalf("err = %v, want class %s attempts %d", err, tc.wantClass, tc.wantReqs)
			}
		})
	}
}

// F30-31：每次嘗試各自的期限；父 ctx 仍活著時單次期限到可重試.
func TestClientPerAttemptTimeoutRetriedWhenParentAlive(t *testing.T) {
	captureLogs(t)
	ts := newTestServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			blockUntilDone(r)
			return
		}
		okJSON(w)
	})
	c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) { o.AttemptTimeout = 80 * time.Millisecond })
	start := time.Now()
	out, err := getOK(c, warnSpec)
	if err != nil || !out.OK || ts.count() != 2 {
		t.Fatalf("err=%v out=%+v reqs=%d", err, out, ts.count())
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %v; attempts must not share one deadline", el)
	}
}

// F30-31：父 ctx 本身到期 → 不重試、不等待；Sleep 0 次.
func TestClientParentDeadlineNotRetried(t *testing.T) {
	captureLogs(t)
	var sleeps atomic.Int32
	ts := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { blockUntilDone(r) })
	c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) {
		o.AttemptTimeout = 5 * time.Second
		o.Sleep = func(context.Context, time.Duration) error { sleeps.Add(1); return nil }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var out decoded
	err := c.GetJSON(ctx, warnSpec, "x", nil, &out)
	var ce *Error
	if !errors.As(err, &ce) || ce.Attempts != 1 || ce.Class != "timeout" {
		t.Fatalf("err = %v, want *Error timeout with 1 attempt", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is DeadlineExceeded must hold: %v", err)
	}
	if sleeps.Load() != 0 || ts.count() != 1 {
		t.Fatalf("sleeps=%d requests=%d, want 0 and 1", sleeps.Load(), ts.count())
	}
}

// F30-31：等待期間取消 → 立刻回 ctx 錯誤，只有 1 次請求.
func TestClientStopsRetryOnContextCancel(t *testing.T) {
	captureLogs(t)
	ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) {
		o.Backoff = 5 * time.Second
		o.Sleep = nil // 用真正的 retry.SleepCtx.
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelledAt time.Time
	time.AfterFunc(50*time.Millisecond, func() { cancelledAt = time.Now(); cancel() })
	var out decoded
	err := c.GetJSON(ctx, warnSpec, "x", nil, &out)
	returned := time.Now()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if returned.Sub(cancelledAt) > 100*time.Millisecond {
		t.Fatalf("returned %v after cancel", returned.Sub(cancelledAt))
	}
	if ts.count() != 1 {
		t.Fatalf("requests = %d, want 1", ts.count())
	}
	if got := ClassOf(err); got != "network" {
		t.Fatalf("canceled class = %q, want network", got)
	}
}

// F30-31：授權碼只走 header；log 與錯誤字串都不含它.
func TestClientAuthHeaderOnly(t *testing.T) {
	logs := captureLogs(t)
	ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	c := newTestClient(ts, nil, testSettings(nil), nil)
	_, err := getOK(c, warnSpec)
	if err == nil {
		t.Fatal("want error")
	}
	if got, _ := ts.lastAuth.Load().(string); got != testAuthKey {
		t.Fatalf("Authorization header = %q", got)
	}
	if u, _ := ts.lastURL.Load().(string); strings.Contains(u, "SECRET") || strings.Contains(strings.ToLower(u), "authorization") {
		t.Fatalf("auth key in URL: %s", u)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("auth key leaked: err=%v logs=%s", err, logs)
	}
}

// F30-31：重試 log 欄位.
func TestClientRetryLogFields(t *testing.T) {
	logs := captureLogs(t)
	ts := newTestServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		okJSON(w)
	})
	c := newTestClient(ts, nil, testSettings(nil), nil)
	if _, err := getOK(c, warnSpec); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, `msg="cwa fetch retried"`) || !strings.Contains(out, "dataset=W-C0033-001") ||
		!strings.Contains(out, "attempts=2") || !strings.Contains(out, "ok=true") {
		t.Fatalf("retry log missing fields: %s", out)
	}
	for _, bad := range []string{"http://", "?format=", "127.0.0.1"} {
		if strings.Contains(out, bad) {
			t.Fatalf("retry log leaks %q: %s", bad, out)
		}
	}
	// 正常一次成功不得出現該行.
	logs.Reset()
	ts2 := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
	if _, err := getOK(newTestClient(ts2, nil, testSettings(nil), nil), warnSpec); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "cwa fetch retried") {
		t.Fatalf("clean success must not log a retry: %s", logs)
	}
	// 最終失敗也要印（ok=false）.
	logs.Reset()
	ts3 := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	_, _ = getOK(newTestClient(ts3, nil, testSettings(nil), nil), warnSpec)
	if !strings.Contains(logs.String(), "ok=false") {
		t.Fatalf("final failure after retry must log ok=false: %s", logs)
	}
}

// F30-31：回應上限；恰等於上限成功、多 1 byte 失敗.
func TestClientBodyLimit(t *testing.T) {
	const limit = 100
	payload := func(n int) string {
		body := `{"ok":true}`
		return body + strings.Repeat(" ", n-len(body))
	}
	tests := []struct {
		name      string
		size      int
		wantClass string
		wantReqs  int
	}{
		{"exactly at limit succeeds", limit, "", 1},
		{"one byte over fails", limit + 1, "too_large", 1},
		{"far over fails without retry", limit * 50, "too_large", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(payload(tc.size)))
			})
			c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) { o.MaxBody = limit })
			out, err := getOK(c, warnSpec)
			if tc.wantClass == "" {
				if err != nil || !out.OK {
					t.Fatalf("err=%v out=%+v", err, out)
				}
			} else if ClassOf(err) != tc.wantClass || out.OK {
				t.Fatalf("err=%v out=%+v want class %s", err, out, tc.wantClass)
			}
			if ts.count() != tc.wantReqs {
				t.Fatalf("requests = %d", ts.count())
			}
		})
	}
}

// F30-31.
func TestFetchBudget(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		o        ClientOptions
		want     time.Duration
	}{
		{"defaults 2x10+1", 2, ClientOptions{}, 21 * time.Second},
		{"max_attempts 5", 5, ClientOptions{}, 54 * time.Second},
		{"forecast 2x30+1", 2, ClientOptions{AttemptTimeout: ForecastAttemptTimeout}, 61 * time.Second},
		{"single attempt has no backoff", 1, ClientOptions{}, 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testSettings(func(c *config.Config) { c.CWA.MaxAttempts = tc.attempts })
			if got := NewClient(tc.o, nil, s).FetchBudget(); got != tc.want {
				t.Fatalf("FetchBudget = %v, want %v", got, tc.want)
			}
		})
	}
}
