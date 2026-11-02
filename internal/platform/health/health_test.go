package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
