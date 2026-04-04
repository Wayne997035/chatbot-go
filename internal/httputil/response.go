package httputil

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// Response 統一回應結構.
type Response struct {
	Status *Status `json:"status,omitempty"`
	Data   any     `json:"data,omitempty"`
}

// Status 狀態結構.
type Status struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OK 成功回應.
func OK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, data)
}

// Success 成功回應帶訊息.
func Success(c echo.Context, message string) error {
	return c.JSON(http.StatusOK, Response{
		Status: &Status{Code: "200", Message: message},
	})
}
