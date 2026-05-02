package weather

import (
	"chatbot-go/internal/platform/config"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

func TestTriggerSyncRejectsConcurrentRun(t *testing.T) {
	h := NewWeatherHandler(nil, &config.Config{})
	h.syncing.Store(true)

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
	rec := httptest.NewRecorder()

	if err := h.TriggerSync(e.NewContext(req, rec)); err != nil {
		t.Fatalf("TriggerSync() error = %v", err)
	}

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestTriggerSyncUsesBackgroundContext(t *testing.T) {
	done := make(chan error, 1)
	h := NewWeatherHandler(nil, &config.Config{})
	h.syncFunc = func(ctx context.Context, _, _ string, _ int, _ weatherstore.WeatherRepository) error {
		done <- ctx.Err()
		return nil
	}

	e := echo.New()
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
	rec := httptest.NewRecorder()

	if err := h.TriggerSync(e.NewContext(req, rec)); err != nil {
		t.Fatalf("TriggerSync() error = %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if err := <-done; err != nil {
		t.Fatalf("background sync context already canceled: %v", err)
	}
}
