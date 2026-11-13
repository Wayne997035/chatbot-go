package weather

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
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
	h.syncFunc = func(ctx context.Context, _ *cwa.Client, _ int, _ weatherstore.WeatherRepository) (SyncResult, error) {
		done <- ctx.Err()
		return SyncResult{}, nil
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

// 關機時取消進行中的手動同步（可能要好幾分鐘），等它收尾；之後不再接受新的同步.
func TestStopCancelsTheManualSync(t *testing.T) {
	h := NewWeatherHandler(nil, &config.Config{})
	started := make(chan struct{})
	result := make(chan error, 1)
	h.syncFunc = func(ctx context.Context, _ *cwa.Client, _ int, _ weatherstore.WeatherRepository) (SyncResult, error) {
		close(started)
		<-ctx.Done()
		result <- ctx.Err()
		return SyncResult{}, ctx.Err()
	}
	trigger := func() int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
		rec := httptest.NewRecorder()
		if err := h.TriggerSync(echo.New().NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		return rec.Code
	}
	if code := trigger(); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	<-started

	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Error("the sync must see its context canceled")
		}
	default:
		t.Fatal("Stop returned before the sync finished")
	}
	if code := trigger(); code != http.StatusServiceUnavailable || h.syncing.Load() {
		t.Errorf("after stop: status %d, syncing %v", code, h.syncing.Load())
	}
}
