package app

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// F30-56：手動觸發預報同步用的是 app 那份共用記帳（cwaLedger），不是預設客戶端的私有記帳.
func TestBuildWiresForecastClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":"true","records":{"locations":[]}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "fcst-test-key"}}
	a, err := build(cfg, unreachableDB(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.weatherHandler.Stop(context.Background()) })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
	rec := httptest.NewRecorder()
	if err := a.weatherHandler.TriggerSync(echo.New().NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.cwaLedger.Usage(context.Background()).ByStream[cwa.StreamForecast] >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("app ledger saw no forecast usage: %+v", a.cwaLedger.Usage(context.Background()))
}
