package middleware

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/config"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// LineSignatureValidator LINE Webhook 簽名驗證 middleware.
func LineSignatureValidator() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			signature := c.Request().Header.Get("X-Line-Signature")
			if signature == "" {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Missing X-Line-Signature header"))
			}

			body, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidParameter, "Failed to read body"))
			}
			// 回寫 body 供 handler 讀取
			c.Request().Body = io.NopCloser(bytes.NewReader(body))

			cfg := config.Get()
			if !validateHMAC(body, signature, cfg.Line.ChannelSecret) {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Invalid signature"))
			}

			return next(c)
		}
	}
}

func validateHMAC(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
