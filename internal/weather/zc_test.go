package weather

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// zcCountingServer 回合法預報 JSON 並計數的假氣象署.
func zcCountingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	body, err := json.Marshal(testCWAResponse())
	if err != nil {
		t.Fatalf("marshal test response: %v", err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// [F30-81] 手動觸發天氣同步用的是注入的客戶端（WithCWAClient），不是依設定自建的預設客戶端.
func TestManualTriggerUsesInjectedClient(t *testing.T) {
	srvA, hitsA := zcCountingServer(t) // 注入的客戶端指向它.
	srvB, hitsB := zcCountingServer(t) // 設定檔的 CWABaseURL 指向它：預設客戶端才會打到.

	cfg := &config.Config{Weather: config.WeatherConfig{CWABaseURL: srvB.URL, CWAAuthKey: "test-key"}}
	h := NewWeatherHandler(&fakeWeatherRepo{}, cfg).WithCWAClient(testForecastClient(srvA.URL, "test-key", nil))
	t.Cleanup(func() {
		if err := h.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
	rec := httptest.NewRecorder()
	if err := h.TriggerSync(echo.New().NewContext(req, rec)); err != nil {
		t.Fatalf("TriggerSync() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// 回 200 時背景同步還沒跑完：等 syncing 變回 false 才讀請求數.
	deadline := time.Now().Add(10 * time.Second)
	for h.syncing.Load() {
		if time.Now().After(deadline) {
			t.Fatal("manual sync still running after 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := hitsA.Load(); got != cwa.ForecastDatasetCount {
		t.Errorf("injected client server got %d requests, want %d", got, cwa.ForecastDatasetCount)
	}
	if got := hitsB.Load(); got != 0 {
		t.Errorf("config-URL server got %d requests, want 0 (a default client was built instead of the injected one)", got)
	}
}

// [F30-83] 預報每份之間的 rate-limit 等待：d>0 真的等、ctx 取消立刻回、d<=0 不等.
func TestForecastSleepWaitsAndHonoursCancel(t *testing.T) {
	sleep := forecastSleep // 預設實作（其他測試換掉的版本都會在 Cleanup 還原）.

	t.Run("positive d waits", func(t *testing.T) {
		start := time.Now()
		if err := sleep(context.Background(), 30*time.Millisecond); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got := time.Since(start); got < 30*time.Millisecond {
			t.Errorf("returned after %v, want >= 30ms", got)
		}
	})

	t.Run("cancel returns promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		start := time.Now()
		err := sleep(ctx, 2*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if got := time.Since(start); got >= 500*time.Millisecond {
			t.Errorf("returned after %v, want < 500ms", got)
		}
	})

	t.Run("zero d does not wait", func(t *testing.T) {
		start := time.Now()
		if err := sleep(context.Background(), 0); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got := time.Since(start); got >= 100*time.Millisecond {
			t.Errorf("returned after %v, want < 100ms", got)
		}
	})

	t.Run("zero d with canceled ctx reports it", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := sleep(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}
