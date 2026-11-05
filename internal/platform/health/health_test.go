package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func ready(t *testing.T, checks ...Check) (status int, body map[string]any) {
	t.Helper()
	e := echo.New()
	e.GET("/ready", NewHealthHandler(checks...).Ready)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", http.NoBody))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("error details must not be returned: %s", rec.Body.String())
	}
	return rec.Code, body
}

func ok(context.Context) error   { return nil }
func down(context.Context) error { return errors.New("dial tcp 10.0.0.5:27017: connection refused") }

func TestReady(t *testing.T) {
	cases := []struct {
		name   string
		checks []Check
		code   int
		status string
	}{
		{"all up", []Check{{Name: "mongo", Required: true, Ping: ok}, {Name: "redis", Ping: ok}}, http.StatusOK, "ok"},
		{"redis down is degraded", []Check{{Name: "mongo", Required: true, Ping: ok}, {Name: "redis", Ping: down}}, http.StatusOK, "degraded"},
		{"mongo down is unavailable", []Check{{Name: "mongo", Required: true, Ping: down}, {Name: "redis", Ping: ok}}, http.StatusServiceUnavailable, "unavailable"},
		{"both down", []Check{{Name: "mongo", Required: true, Ping: down}, {Name: "redis", Ping: down}}, http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, body := ready(t, tt.checks...)
			if code != tt.code || body["status"] != tt.status {
				t.Errorf("code=%d body=%v, want %d %s", code, body, tt.code, tt.status)
			}
		})
	}
}

func TestReadyGivesUpAfterTheTimeout(t *testing.T) {
	slow := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	code, _ := ready(t, Check{Name: "mongo", Required: true, Ping: slow})
	if code != http.StatusServiceUnavailable {
		t.Errorf("a hanging dependency must fail within the timeout, got %d", code)
	}
}

// fakeClock [F24-81] 可推進的假時鐘；起點 MUST NOT 是 Unix 0（0 代表從未寫過）.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func getHealth(t *testing.T, ctx context.Context, h *HealthHandler) (code int, body map[string]string, raw string) {
	t.Helper()
	e := echo.New()
	e.GET("/health", h.HealthCheck)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/health", http.NoBody))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

// [F24-81] 不論 /health 被打多少次，保活寫入每 10 分鐘最多一次.
func TestHealthKeepAliveWritesAtMostOncePerInterval(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	writes := 0
	h := NewHealthHandler().WithKeepAlive(func(context.Context) error { writes++; return nil }, clock.Now)

	steps := []struct {
		name       string
		advance    time.Duration
		requests   int
		wantWrites int
	}{
		{"three calls at once write once", 0, 3, 1},
		{"just under 10 minutes still once", 10*time.Minute - time.Second, 1, 1},
		{"full 10 minutes writes again", time.Second, 1, 2},
		{"again right after stays at two", 0, 2, 2},
	}
	for _, st := range steps {
		clock.Advance(st.advance)
		for range st.requests {
			if code, _, _ := getHealth(t, context.Background(), h); code != http.StatusOK {
				t.Fatalf("%s: /health = %d", st.name, code)
			}
		}
		if writes != st.wantWrites {
			t.Errorf("%s: writes = %d, want %d", st.name, writes, st.wantWrites)
		}
	}
}

// [F24-81] 寫入失敗只記 log，/health 照回 200，回應不含錯誤細節，且失敗也算一次嘗試.
func TestHealthKeepAliveFailureStillReportsOK(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	calls := 0
	failing := func(context.Context) error {
		calls++
		return errors.New("dial tcp 10.0.0.5:6379: connection refused")
	}
	h := NewHealthHandler().WithKeepAlive(failing, clock.Now)

	for i := range 2 {
		code, body, raw := getHealth(t, context.Background(), h)
		if code != http.StatusOK || body["status"] != "ok" {
			t.Errorf("call %d: code=%d body=%v", i, code, body)
		}
		if strings.Contains(raw, "refused") || strings.Contains(raw, "6379") {
			t.Errorf("call %d: error details leaked: %s", i, raw)
		}
	}
	if calls != 1 {
		t.Errorf("a failed write must still count as an attempt: calls = %d, want 1", calls)
	}
}

// [F24-81] 客戶端斷線（請求 context 已取消）不得取消保活寫入；now 傳 nil 時改用 time.Now.
func TestHealthKeepAliveSurvivesClientCancel(t *testing.T) {
	calls := 0
	var ctxErr error
	var hasDeadline bool
	h := NewHealthHandler().WithKeepAlive(func(ctx context.Context) error {
		calls++
		ctxErr = ctx.Err()
		_, hasDeadline = ctx.Deadline()
		return nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _, _ := getHealth(t, ctx, h); code != http.StatusOK {
		t.Fatalf("/health = %d", code)
	}
	if calls != 1 {
		t.Fatalf("write calls = %d, want 1", calls)
	}
	if ctxErr != nil {
		t.Errorf("write ctx must not be canceled: %v", ctxErr)
	}
	if !hasDeadline {
		t.Error("write ctx must carry the timeout")
	}
}

// [F24-81] write 為 nil 不啟用保活，/health 照常.
func TestHealthKeepAliveDisabledWithoutWriter(t *testing.T) {
	h := NewHealthHandler().WithKeepAlive(nil, nil)
	if code, body, _ := getHealth(t, context.Background(), h); code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("code=%d body=%v", code, body)
	}
}
