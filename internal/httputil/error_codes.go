package httputil

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
)

const (
	ErrorCodeInvalidSignature = 1001
	ErrorCodeInvalidParameter = 2001
	ErrorCodeNotFound         = 4001
	ErrorCodeWeatherNotFound  = 4002
	ErrorCodeServerError      = 5001
)

var errorMessages = map[int]string{
	ErrorCodeInvalidSignature: "Invalid LINE signature",
	ErrorCodeInvalidParameter: "Invalid parameter",
	ErrorCodeNotFound:         "Resource not found",
	ErrorCodeWeatherNotFound:  "Weather data not found",
	ErrorCodeServerError:      "Internal server error",
}

// ErrorCodeResponse 取得錯誤碼對應的訊息.
func ErrorCodeResponse(code int) string {
	if msg, ok := errorMessages[code]; ok {
		return msg
	}
	return "Unknown error"
}

// ErrorWithCode 建立錯誤回應.
func ErrorWithCode(code int, message string) Response {
	return Response{
		Status: &Status{
			Code:    fmt.Sprintf("%d", code),
			Message: message,
		},
	}
}

// ErrorHandler Echo 全域錯誤處理.
func ErrorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	he, ok := err.(*echo.HTTPError)
	if ok {
		_ = c.JSON(he.Code, ErrorWithCode(he.Code, fmt.Sprintf("%v", he.Message)))
		return
	}

	_ = c.JSON(http.StatusInternalServerError, ErrorWithCode(ErrorCodeServerError, "internal server error"))
}
