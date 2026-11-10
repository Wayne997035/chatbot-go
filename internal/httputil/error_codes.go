package httputil

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

const (
	ErrorCodeInvalidSignature = 1001
	ErrorCodeInvalidParameter = 2001
	ErrorCodeNotFound         = 4001
	ErrorCodeWeatherNotFound  = 4002
	ErrorCodeConflict         = 4009
	ErrorCodeDataErased       = 4010
	ErrorCodeServerError      = 5001
	// ErrorCodeEraseIncomplete [F293-03] 使用者刪除資料之後才落地的寫入，補刪沒做成：資料可能還在，要照實告知.
	ErrorCodeEraseIncomplete = 5002
)

var errorMessages = map[int]string{
	ErrorCodeInvalidSignature: "Invalid LINE signature",
	ErrorCodeInvalidParameter: "Invalid parameter",
	ErrorCodeNotFound:         "Resource not found",
	ErrorCodeWeatherNotFound:  "Weather data not found",
	ErrorCodeConflict:         "Resource was modified",
	ErrorCodeDataErased:       "Data was erased",
	ErrorCodeServerError:      "Internal server error",
	ErrorCodeEraseIncomplete:  "Data erasure incomplete",
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

	var he *echo.HTTPError
	if errors.As(err, &he) {
		if he.Code >= http.StatusInternalServerError {
			slog.Error("request failed", "route", c.Path(), "status", he.Code, "error", err)
		}
		_ = c.JSON(he.Code, ErrorWithCode(he.Code, fmt.Sprintf("%v", he.Message)))
		return
	}

	// 以前這裡不記錄錯誤：500 發生了卻看不到原因. 記路由樣式（不是實際網址），避免把使用者 ID 寫進 log.
	slog.Error("request failed", "route", c.Path(), "status", http.StatusInternalServerError, "error", err)
	_ = c.JSON(http.StatusInternalServerError, ErrorWithCode(ErrorCodeServerError, "internal server error"))
}
