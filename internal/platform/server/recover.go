package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"

	"github.com/labstack/echo/v5"
)

// panicStackBytes 記錄的堆疊上限（同 v4 Recover 的預設 4 KB）.
const panicStackBytes = 4 << 10

// recoverPanics [F34-73] 自寫的 recover middleware（v5 的 RecoverConfig 沒有 LogErrorFunc）：
// panic → slog 帶路由樣式與堆疊（經 log 出口遮蔽）、不寫 stdout / stderr，回錯誤交給 ErrorHandler 變成 500.
// http.ErrAbortHandler 是 net/http 約定的「中止這個連線」訊號，照 v4 與標準庫行為原樣往上丟.
func recoverPanics() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) (err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				if r == http.ErrAbortHandler { //nolint:errorlint // ErrAbortHandler 約定以值比較（標準庫自己也是 == ）
					panic(r)
				}
				perr, ok := r.(error)
				if !ok {
					perr = fmt.Errorf("%v", r)
				}
				stack := make([]byte, panicStackBytes)
				stack = stack[:runtime.Stack(stack, false)]
				err = logPanic(c, perr, stack)
			}()
			return next(c)
		}
	}
}

// logPanic [F24-94] 把 recover 到的 panic 寫進 slog；path 是路由樣式，不是請求網址.
func logPanic(c *echo.Context, err error, stack []byte) error {
	slog.ErrorContext(c.Request().Context(), "panic recovered", "error", err, "path", c.Path(), "stack", string(stack))
	return err
}
