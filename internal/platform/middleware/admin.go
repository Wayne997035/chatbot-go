package middleware

import (
	"chatbot-go/internal/httputil"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const adminTokenHeader = "X-Admin-Token"

// AdminTokenValidator 保護管理 API（不是 LINE 呼叫的）；adminToken 為空時一律回 503.
func AdminTokenValidator(adminToken string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if adminToken == "" {
				return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(
					httputil.ErrorCodeServerError, "Admin API is not configured"))
			}

			token := adminTokenFromRequest(c.Request())
			if subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) != 1 {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Invalid admin token"))
			}

			return next(c)
		}
	}
}

func adminTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get(adminTokenHeader)); token != "" {
		return token
	}

	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(auth, bearerPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(auth, bearerPrefix))
	}
	return ""
}
