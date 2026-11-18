package httputil

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
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

// statusMessage [F34-72] 回應 body 的 message：*echo.HTTPError 用它的 Message，其餘（含 v5 沒有 Message 的內建錯誤值）用標準狀態文字.
func statusMessage(err error, code int) string {
	var he *echo.HTTPError
	if errors.As(err, &he) && he.Message != "" {
		return he.Message
	}
	return http.StatusText(code)
}

// ErrorHandler Echo 全域錯誤處理.
func ErrorHandler(c *echo.Context, err error) {
	// [F34-72] v5 的 c.Response() 是 http.ResponseWriter，committed 要由 UnwrapResponse 取得.
	if resp, unwrapErr := echo.UnwrapResponse(c.Response()); unwrapErr == nil && resp.Committed {
		return
	}

	// [F34-72] 狀態碼由 echo.StatusCode 取得（回 0 = 沒有狀態碼，走下面的 500）.
	// v5 的 ErrNotFound / ErrMethodNotAllowed / ErrStatusRequestEntityTooLarge 沒有 Message，
	// 文字對映成與 v4 相同的標準狀態文字（"Not Found" 等）；*echo.HTTPError 照舊用它自己的 Message.
	if code := echo.StatusCode(err); code != 0 {
		if code >= http.StatusInternalServerError {
			slog.Error("request failed", "route", c.Path(), "status", code, "error", err)
		}
		_ = c.JSON(code, ErrorWithCode(code, statusMessage(err, code)))
		return
	}

	// 以前這裡不記錄錯誤：500 發生了卻看不到原因. 記路由樣式（不是實際網址），避免把使用者 ID 寫進 log.
	slog.Error("request failed", "route", c.Path(), "status", http.StatusInternalServerError, "error", err)
	_ = c.JSON(http.StatusInternalServerError, ErrorWithCode(ErrorCodeServerError, "internal server error"))
}
