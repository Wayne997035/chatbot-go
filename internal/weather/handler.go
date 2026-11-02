package weather

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/config"
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

type syncFunc func(context.Context, string, string, int, weatherstore.WeatherRepository) error

// WeatherHandler 天氣 API 處理器.
type WeatherHandler struct {
	weatherRepo weatherstore.WeatherRepository
	cfg         *config.Config
	syncing     atomic.Bool
	syncFunc    syncFunc
}

// NewWeatherHandler 建立天氣處理器.
func NewWeatherHandler(weatherRepo weatherstore.WeatherRepository, cfg *config.Config) *WeatherHandler {
	return &WeatherHandler{
		weatherRepo: weatherRepo,
		cfg:         cfg,
		syncFunc:    FetchAndStore,
	}
}

// TriggerSync 手動觸發天氣資料同步.
func (h *WeatherHandler) TriggerSync(c echo.Context) error {
	if !h.syncing.CompareAndSwap(false, true) {
		return c.JSON(http.StatusConflict, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "Weather sync already running"))
	}

	go func() {
		defer h.syncing.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := h.syncFunc(
			ctx,
			h.cfg.Weather.CWABaseURL,
			h.cfg.Weather.CWAAuthKey,
			h.cfg.Weather.RateLimitMs,
			h.weatherRepo,
		); err != nil {
			slog.Error("manual weather sync failed", "error", err)
		}
	}()

	return c.JSON(http.StatusOK, httputil.Response{
		Status: &httputil.Status{Code: "200", Message: "Weather sync triggered"},
	})
}
