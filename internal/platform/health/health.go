package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// readyTimeout 每次 readiness 檢查的時間上限.
const readyTimeout = 2 * time.Second

// statusOK 正常時的狀態.
const statusOK = "ok"

// Check 一項依賴的檢查.
type Check struct {
	Name string
	// Required 必要依賴失敗時 /ready 回 503；非必要的只標記 degraded（例如 Redis：沒有它也能服務，只是功能變少）.
	Required bool
	Ping     func(ctx context.Context) error
}

// HealthHandler 健康檢查處理器.
type HealthHandler struct {
	checks []Check
}

// NewHealthHandler 建立健康檢查處理器；checks 用於 /ready.
func NewHealthHandler(checks ...Check) *HealthHandler {
	return &HealthHandler{checks: checks}
}

// HealthCheck GET /health：程式還活著（liveness）. 不檢查依賴，資料庫掛了重啟程式也沒用.
func (h *HealthHandler) HealthCheck(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": statusOK})
}

// Ready GET /ready：依賴是否可用（readiness）. 必要依賴失敗回 503. 回應只有狀態，錯誤細節只寫進 log.
func (h *HealthHandler) Ready(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), readyTimeout)
	defer cancel()
	status, code := statusOK, http.StatusOK
	results := make(map[string]string, len(h.checks))
	for _, check := range h.checks {
		if err := check.Ping(ctx); err != nil {
			slog.Warn("readiness check failed", "check", check.Name, "error", err)
			if check.Required {
				results[check.Name] = "fail"
				status, code = "unavailable", http.StatusServiceUnavailable
				continue
			}
			results[check.Name] = "degraded"
			if status == statusOK {
				status = "degraded"
			}
			continue
		}
		results[check.Name] = statusOK
	}
	return c.JSON(code, map[string]any{"status": status, "checks": results})
}
