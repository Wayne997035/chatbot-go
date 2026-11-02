package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	go func() { done <- serve(newEcho(), taken.Addr().String(), make(chan os.Signal)) }()
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
	stop := make(chan os.Signal, 1)
	e := newEcho()
	done := make(chan error, 1)
	go func() { done <- serve(e, "127.0.0.1:0", stop) }()
	stop <- os.Interrupt
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
	stop := make(chan os.Signal, 1)
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
	stop <- os.Interrupt
	<-served

	var started sync.WaitGroup
	started.Add(2)
	release := make(chan struct{})
	go func() {
		started.Wait() // 兩個都開始了才放行：依序執行的話會卡住
		close(release)
	}()
	httpStillOpen := make(chan bool, 2)
	worker := func(err error) stopper {
		return stopper{name: "work", stop: func(ctx context.Context) error {
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
	go func() { done <- shutdown(e, []stopper{worker(nil), worker(errors.New("boom"))}) }()
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
