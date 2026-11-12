package weather

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

type syncFunc func(context.Context, *cwa.Client, int, weatherstore.WeatherRepository) (SyncResult, error)

// WeatherHandler 天氣 API 處理器.
type WeatherHandler struct {
	weatherRepo weatherstore.WeatherRepository
	cfg         *config.Config
	syncing     atomic.Bool
	syncFunc    syncFunc
	work        *background.Group // 手動觸發的同步

	clientOnce sync.Once
	client     *cwa.Client
}

// NewWeatherHandler 建立天氣處理器.
func NewWeatherHandler(weatherRepo weatherstore.WeatherRepository, cfg *config.Config) *WeatherHandler {
	return &WeatherHandler{
		weatherRepo: weatherRepo,
		cfg:         cfg,
		syncFunc:    FetchAndStore,
		work:        background.New(0, 0),
	}
}

// WithCWAClient 注入氣象署客戶端（與排程共用 app 的記帳）；沒注入時用預設客戶端 [F30-53].
func (h *WeatherHandler) WithCWAClient(c *cwa.Client) *WeatherHandler {
	h.client = c
	return h
}

func (h *WeatherHandler) cwaClient() *cwa.Client {
	h.clientOnce.Do(func() {
		if h.client == nil {
			h.client = newForecastClient(h.cfg)
		}
	})
	return h.client
}

// Stop 取消進行中的手動同步並等它收尾（資料下次同步會再抓）.
func (h *WeatherHandler) Stop(ctx context.Context) error {
	return stopNow(ctx, h.work)
}

// TriggerSync 手動觸發天氣資料同步.
func (h *WeatherHandler) TriggerSync(c echo.Context) error {
	if !h.syncing.CompareAndSwap(false, true) {
		return c.JSON(http.StatusConflict, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "Weather sync already running"))
	}

	started := h.work.Go(func(ctx context.Context) {
		defer h.syncing.Store(false)

		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		// 手動觸發不看資料新鮮度，一律抓完整一輪 [F30-53].
		if _, err := h.syncFunc(ctx, h.cwaClient(), h.cfg.Weather.RateLimitMs, h.weatherRepo); err != nil {
			slog.Error("manual weather sync failed", errAttr(err))
		}
	})
	if !started {
		h.syncing.Store(false)
		return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(httputil.ErrorCodeServerError, "Shutting down"))
	}

	return c.JSON(http.StatusOK, httputil.Response{
		Status: &httputil.Status{Code: "200", Message: "Weather sync triggered"},
	})
}
