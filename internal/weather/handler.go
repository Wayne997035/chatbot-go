package weather

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/config"
	"net/http"

	"github.com/labstack/echo/v4"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// WeatherHandler 天氣 API 處理器.
type WeatherHandler struct {
	weatherRepo weatherstore.WeatherRepository
	cfg         *config.Config
}

// NewWeatherHandler 建立天氣處理器.
func NewWeatherHandler(weatherRepo weatherstore.WeatherRepository, cfg *config.Config) *WeatherHandler {
	return &WeatherHandler{
		weatherRepo: weatherRepo,
		cfg:         cfg,
	}
}

// TriggerSync 手動觸發天氣資料同步.
func (h *WeatherHandler) TriggerSync(c echo.Context) error {
	go func() {
		ctx := c.Request().Context()
		_ = FetchAndStore(ctx, h.cfg.Weather.CWABaseURL, h.cfg.Weather.CWAAuthKey, h.cfg.Weather.RateLimitMs, h.weatherRepo)
	}()
	return c.JSON(http.StatusOK, httputil.Response{
		Status: &httputil.Status{Code: "200", Message: "Weather sync triggered"},
	})
}
