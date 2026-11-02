package liff

import (
	"chatbot-go/internal/httputil"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const userIDKey = "liffUserID"

// Auth 驗證 Authorization: Bearer <LIFF ID token>，通過後把 user ID 放進 context.
func Auth(v *Verifier) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
			if !ok {
				return unauthorized(c)
			}
			userID, err := v.Verify(c.Request().Context(), token)
			if err != nil {
				if !errors.Is(err, ErrInvalidToken) {
					slog.Warn("LIFF token verification unavailable", "error", err)
					return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(
						httputil.ErrorCodeServerError, "Verification temporarily unavailable"))
				}
				return unauthorized(c)
			}
			c.Set(userIDKey, userID)
			return next(c)
		}
	}
}

func unauthorized(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(httputil.ErrorCodeInvalidSignature, "Please sign in again"))
}

// userID 取得已驗證的使用者 ID.
func userID(c echo.Context) string {
	id, _ := c.Get(userIDKey).(string)
	return id
}
