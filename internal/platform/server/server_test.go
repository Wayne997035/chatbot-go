package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
