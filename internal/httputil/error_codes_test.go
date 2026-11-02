package httputil

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// 500 要留下原因（以前完全不記錄）；只記路由樣式，不記含使用者 ID 的實際網址.
func TestErrorHandlerLogsServerErrors(t *testing.T) {
	logs := captureLogs(t)
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler
	e.GET("/users/:id", func(echo.Context) error { return errors.New("mongo: connection refused") })
	e.GET("/missing", func(echo.Context) error { return echo.NewHTTPError(http.StatusNotFound, "nope") })

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/Usecret123", http.NoBody))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "mongo") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "mongo: connection refused") || !strings.Contains(out, "/users/:id") || strings.Contains(out, "Usecret123") {
		t.Errorf("log = %s", out)
	}

	logs.Reset()
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/missing", http.NoBody))
	if rec.Code != http.StatusNotFound || logs.Len() != 0 {
		t.Errorf("4xx are expected outcomes, not logged as errors: %d %s", rec.Code, logs.String())
	}
}
