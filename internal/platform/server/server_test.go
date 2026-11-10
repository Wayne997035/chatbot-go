package server

import (
	"bytes"
	"chatbot-go/internal/platform/background"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestNewEchoHasTimeoutsLimitsAndSecurityHeaders(t *testing.T) {
	e := newEcho()
	if e.Server.ReadHeaderTimeout == 0 || e.Server.ReadTimeout == 0 || e.Server.WriteTimeout == 0 || e.Server.IdleTimeout == 0 {
		t.Fatalf("server timeouts must be set: %+v", e.Server)
	}
	e.POST("/echo", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/echo", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for header, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	big := strings.NewReader(strings.Repeat("x", 3<<20))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/echo", big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d", rec.Code)
	}
}

// port 被佔用時要回傳錯誤（讓程式結束、讓容器重啟），不能活著卻沒在服務.
func TestServeReturnsWhenThePortIsTaken(t *testing.T) {
	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	done := make(chan error, 1)
	go func() { done <- serve(newEcho(), taken.Addr().String(), make(chan struct{})) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error for a port that is already in use")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept running although the server could not start")
	}
}

func TestServeStopsOnSignal(t *testing.T) {
	stop := make(chan struct{})
	e := newEcho()
	done := make(chan error, 1)
	go func() { done <- serve(e, "127.0.0.1:0", stop) }()
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a normal stop is not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the stop signal")
	}
	_ = e.Close()
}

// 請求日誌記到的狀態碼要是真的（以前 handler 回錯誤時一律記成 200）.
func TestRequestLogRecordsTheRealStatus(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEcho()
	e.GET("/boom", func(echo.Context) error { return errors.New("db down") })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", http.NoBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	// 只看請求日誌那一行（錯誤處理器另外會記一行 status=500，不能拿來混充）.
	var requestLine string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "msg=request ") {
			requestLine = line
		}
	}
	if !strings.Contains(requestLine, "status=500") {
		t.Errorf("the request log line must show status=500, got %q", requestLine)
	}
}

func realIP(t *testing.T, extractor echo.IPExtractor, remote, xff string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	return extractor(req)
}

// 沒設定代理時不相信 header（偽造無效）；設定後只相信來自代理的 header，才不會所有人共用一個限流額度.
func TestIPExtractor(t *testing.T) {
	direct, err := ipExtractor("")
	if err != nil {
		t.Fatal(err)
	}
	if got := realIP(t, direct, "198.51.100.9:5000", "1.2.3.4"); got != "198.51.100.9" {
		t.Errorf("without trusted proxies the header is ignored, got %s", got)
	}

	behindProxy, err := ipExtractor("10.0.0.0/8, 172.16.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	if got := realIP(t, behindProxy, "10.1.2.3:443", "203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("through the proxy the client IP is used, got %s", got)
	}
	if got := realIP(t, behindProxy, "198.51.100.9:5000", "1.2.3.4"); got != "198.51.100.9" {
		t.Errorf("a client that is not the proxy cannot spoof the header, got %s", got)
	}
	// Echo 預設信任所有內網位址；只有設定的代理範圍才能代填 header，同一個內網的其他主機不行.
	for _, other := range []string{"192.168.1.50:5000", "127.0.0.1:5000"} {
		if got := realIP(t, behindProxy, other, "1.2.3.4"); got == "1.2.3.4" {
			t.Errorf("%s is not a configured proxy and must not set the client IP", other)
		}
	}
	if _, err := ipExtractor("10.0.0.0/8,not-a-cidr"); err == nil {
		t.Error("an invalid entry must stop startup")
	}
}

// 關機順序：先停止接收 HTTP，再「同時」停止各項背景工作，錯誤全部回報.
func TestShutdownStopsHTTPFirstThenStopsBackgroundWorkInParallel(t *testing.T) {
	e := newEcho()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() { served <- serve(e, "127.0.0.1:0", stop) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.ListenerAddr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	addr := e.ListenerAddr().String()
	close(stop)
	<-served

	var started sync.WaitGroup
	started.Add(2)
	release := make(chan struct{})
	go func() {
		started.Wait() // 兩個都開始了才放行：依序執行的話會卡住
		close(release)
	}()
	httpStillOpen := make(chan bool, 2)
	worker := func(err error) Stopper {
		return Stopper{Name: "work", Stop: func(ctx context.Context) error {
			conn, dialErr := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
			if dialErr == nil {
				_ = conn.Close()
			}
			httpStillOpen <- dialErr == nil
			started.Done()
			<-release
			return err
		}}
	}

	done := make(chan error, 1)
	go func() { done <- shutdown(context.Background(), e, []Stopper{worker(nil), worker(errors.New("boom"))}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("errors are reported: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background work must be stopped in parallel")
	}
	for range 2 {
		if <-httpStillOpen {
			t.Error("HTTP must stop accepting before background work is stopped")
		}
	}
}

// ctx 取消（或收到關機訊號）時 Run 停止 HTTP、停止背景工作後才返回.
func TestRunStopsWhenTheContextIsCanceled(t *testing.T) {
	e := newEcho()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, e, "127.0.0.1:0", []Stopper{{Name: "work", Stop: func(stopCtx context.Context) error {
			stopped <- stopCtx.Err() // 關機的期限不能沿用已取消的 ctx，否則處理中的工作會立刻被取消
			return nil
		}}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.ListenerAddr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a normal stop is not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was canceled")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("background work got an already canceled context: %v", err)
		}
	default:
		t.Error("background work must be stopped before Run returns")
	}
}

// syncBuffer 以 mutex 保護的 writer：serve 的 goroutine 寫、測試 goroutine 讀.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureServerSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func slogRecords(t *testing.T, buf *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// [F24-94] panic 要經 slog 記錄（含 error 與路由樣式 path），不能由 echo 自己寫 stderr.
func TestRecoverLogsPanicThroughSlog(t *testing.T) {
	buf := captureServerSlog(t)
	e := newEcho()
	e.GET("/boom/:id", func(echo.Context) error { panic("kaboom") })

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom/42", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	recs := slogRecords(t, buf, "panic recovered")
	if len(recs) == 0 {
		t.Fatalf("no 'panic recovered' slog record: %s", buf.String())
	}
	if recs[0]["error"] == nil || recs[0]["path"] != "/boom/:id" {
		t.Errorf("record missing error or route-pattern path: %v", recs[0])
	}
}

// [F24-94] 正式啟動後 http.Server.ErrorLog 要導向 slog（echo 在 Start 時會把它蓋成 e.StdLogger）.
func TestServerErrorLogGoesThroughSlog(t *testing.T) {
	buf := captureServerSlog(t)
	e := newEcho()
	stop := make(chan struct{})
	go func() { _ = serve(e, "127.0.0.1:0", stop) }()
	t.Cleanup(func() {
		close(stop)
		_ = e.Close()
	})

	deadline := time.Now().Add(5 * time.Second)
	for e.ListenerAddr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !e.HidePort {
		t.Error("HidePort must be true so echo does not print the address banner")
	}
	if e.Server.ErrorLog == nil {
		t.Fatal("Server.ErrorLog is nil after Start")
	}
	e.Server.ErrorLog.Print("tls handshake error")
	recs := slogRecords(t, buf, "tls handshake error")
	if len(recs) != 1 {
		t.Fatalf("records = %d: %s", len(recs), buf.String())
	}
	if recs[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", recs[0]["level"])
	}
}

// TestShutdownBudgetUnderPlatformGrace [SEC29-03][F292-03] 關機期限加背景工作寬限必須小於部署平台的 60 秒關機寬限.
func TestShutdownBudgetUnderPlatformGrace(t *testing.T) {
	if shutdownTimeout+background.CancelGrace >= 60*time.Second {
		t.Fatalf("shutdown budget %v must stay under the 60s platform grace", shutdownTimeout+background.CancelGrace)
	}
}
