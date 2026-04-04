package health

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// HealthHandler 健康檢查處理器.
type HealthHandler struct{}

// NewHealthHandler 建立健康檢查處理器.
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// HealthCheck 回傳服務健康狀態.
func (h *HealthHandler) HealthCheck(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
