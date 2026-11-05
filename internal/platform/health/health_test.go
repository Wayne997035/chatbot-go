package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
		h.waitKeepAlive()
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
	h.waitKeepAlive()
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
	h.waitKeepAlive()
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

// waitOrFatal 在測試 goroutine 等 done 關閉，逾時就 Fatal（不卡到 go test 的預設逾時）.
func waitOrFatal(t *testing.T, done <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s did not finish within %s", what, timeout)
	}
}

// [F24-81] 32 個請求同時到，保活只寫一次. 時鐘是柵欄：32 個請求都讀過 lastKeepAlive、
// 都進到 now() 之後才一起放行，任何人 CAS 之前大家手上都是同一個 last.
func TestHealthKeepAliveConcurrent(t *testing.T) {
	const n = 32
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	barrier := func() time.Time {
		arrived.Done()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("barrier clock: not every request reached now() within 5s")
		}
		return base
	}
	var writes atomic.Int32
	h := NewHealthHandler().WithKeepAlive(func(context.Context) error { writes.Add(1); return nil }, barrier)

	var requests sync.WaitGroup
	for range n {
		requests.Add(1)
		go func() {
			defer requests.Done()
			if code, _, _ := getHealth(t, context.Background(), h); code != http.StatusOK {
				t.Errorf("/health = %d", code)
			}
		}()
	}
	finished := make(chan struct{})
	go func() { requests.Wait(); close(finished) }()
	waitOrFatal(t, finished, 5*time.Second, "concurrent /health requests")
	h.waitKeepAlive()
	if got := writes.Load(); got != 1 {
		t.Errorf("writes = %d, want 1", got)
	}
}

// [F24-81] Redis 寫入慢時 /health 不等它.
func TestHealthKeepAliveDoesNotBlockResponse(t *testing.T) {
	unblock := make(chan struct{})
	h := NewHealthHandler().WithKeepAlive(func(context.Context) error { <-unblock; return nil }, nil)
	got := make(chan int, 1)
	go func() {
		code, _, _ := getHealth(t, context.Background(), h)
		got <- code
	}()
	select {
	case code := <-got:
		if code != http.StatusOK {
			t.Errorf("/health = %d", code)
		}
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("/health waited for the keep-alive write")
	}
	close(unblock)
	h.waitKeepAlive()
}

// [F24-81] 時鐘往回調視為到期，保活不會停到時鐘追上.
func TestHealthKeepAliveAfterClockRollback(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	var writes atomic.Int32
	h := NewHealthHandler().WithKeepAlive(func(context.Context) error { writes.Add(1); return nil }, clock.Now)
	getHealth(t, context.Background(), h)
	h.waitKeepAlive()
	clock.Advance(-time.Hour)
	getHealth(t, context.Background(), h)
	h.waitKeepAlive()
	if got := writes.Load(); got != 2 {
		t.Errorf("writes = %d, want 2 (a clock rollback counts as due)", got)
	}
}

// [F24-81] 保活寫入 panic 只記 log，/health 照回 200，程序不結束.
func TestHealthKeepAlivePanicIsContained(t *testing.T) {
	h := NewHealthHandler().WithKeepAlive(func(context.Context) error { panic("boom") }, nil)
	if code, body, _ := getHealth(t, context.Background(), h); code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("code=%d body=%v", code, body)
	}
	waited := make(chan struct{})
	go func() { h.waitKeepAlive(); close(waited) }()
	waitOrFatal(t, waited, 2*time.Second, "keep-alive goroutine")
}

// serveReady 對同一個 handler 打一次 /ready.
func serveReady(ctx context.Context, h *HealthHandler) (code int, body map[string]any, raw string) {
	e := echo.New()
	e.GET("/ready", h.Ready)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ready", http.NoBody))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

// /ready 結果快取 readyCacheTTL：期間內連打只 ping 一次，過期才再 ping.
func TestReadyCachesResultBriefly(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	var pings atomic.Int32
	h := NewHealthHandler(Check{Name: "mongo", Required: true, Ping: func(context.Context) error { pings.Add(1); return nil }})
	h.now = clock.Now
	for range 3 {
		if code, _, _ := serveReady(context.Background(), h); code != http.StatusOK {
			t.Fatalf("/ready = %d", code)
		}
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("pings within TTL = %d, want 1", got)
	}
	clock.Advance(readyCacheTTL)
	serveReady(context.Background(), h)
	if got := pings.Load(); got != 2 {
		t.Errorf("pings after TTL = %d, want 2", got)
	}
}

// /ready 並行請求只 ping 一輪（真實時鐘：時間要在取鎖後才讀）.
func TestReadyConcurrentRequestsPingOnce(t *testing.T) {
	const n = 32
	var pings atomic.Int32
	entered := make(chan struct{}, 2*n)
	unblock := make(chan struct{})
	h := NewHealthHandler(Check{Name: "mongo", Required: true, Ping: func(context.Context) error {
		pings.Add(1)
		entered <- struct{}{}
		<-unblock
		return nil
	}})

	bodies := make(chan string, n)
	var requests sync.WaitGroup
	for range n {
		requests.Add(1)
		go func() {
			defer requests.Done()
			_, _, raw := serveReady(context.Background(), h)
			bodies <- raw
		}()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("no ping started within 2s")
	}
	select {
	case <-entered:
		close(unblock)
		t.Fatal("a second ping started while the first was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	finished := make(chan struct{})
	go func() { requests.Wait(); close(finished) }()
	waitOrFatal(t, finished, 5*time.Second, "concurrent /ready requests")
	if got := pings.Load(); got != 1 {
		t.Errorf("pings = %d, want 1", got)
	}
	close(bodies)
	first := ""
	for b := range bodies {
		if first == "" {
			first = b
		}
		if b != first {
			t.Errorf("responses differ: %q vs %q", first, b)
		}
	}
}

// 第一個請求的客戶端斷線，ping 照常跑完，不把「失敗」快取給所有人.
func TestReadyCacheIgnoresClientCancel(t *testing.T) {
	h := NewHealthHandler(Check{Name: "mongo", Required: true, Ping: func(ctx context.Context) error { return ctx.Err() }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, body, _ := serveReady(ctx, h); code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("code=%d body=%v, want 200 ok", code, body)
	}
}

// 時鐘往回調時快取視為過期，重新 ping.
func TestReadyCacheAfterClockRollback(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	var pings atomic.Int32
	h := NewHealthHandler(Check{Name: "mongo", Required: true, Ping: func(context.Context) error { pings.Add(1); return nil }})
	h.now = clock.Now
	serveReady(context.Background(), h)
	clock.Advance(-time.Hour)
	serveReady(context.Background(), h)
	if got := pings.Load(); got != 2 {
		t.Errorf("pings = %d, want 2 (a clock rollback expires the cache)", got)
	}
}

// ping panic 時鎖也要釋放，之後的 /ready 不會永久卡住.
func TestReadyPingPanicDoesNotWedgeLock(t *testing.T) {
	var panicking atomic.Bool
	panicking.Store(true)
	h := NewHealthHandler(Check{Name: "mongo", Required: true, Ping: func(context.Context) error {
		if panicking.Load() {
			panic("boom")
		}
		return nil
	}})
	func() {
		defer func() { _ = recover() }()
		serveReady(context.Background(), h)
	}()
	panicking.Store(false)
	got := make(chan int, 1)
	go func() {
		code, _, _ := serveReady(context.Background(), h)
		got <- code
	}()
	select {
	case code := <-got:
		if code != http.StatusOK {
			t.Errorf("/ready after a panicking ping = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/ready is stuck after a panicking ping")
	}
}
