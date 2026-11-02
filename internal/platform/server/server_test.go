package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
