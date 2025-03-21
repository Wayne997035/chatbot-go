package webhook

import (
	webhookdomain "chatbot-go/internal/domain/webhook"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type Handler interface {
	WebhookResponse(c echo.Context) error
}

type handler struct {
	// user 提供使用者相關的業務邏輯
	webhook       webhookdomain.Service
	ChannelSecret string

	// logger 用於記錄操作和錯誤
	logger *zap.Logger
}

func NewHandler(
	webhook webhookdomain.Service,
	logger *zap.Logger,
) Handler {
	return &handler{
		webhook: webhook,
		logger:  logger.Named("Handler").Named("Webhook"),
	}
}

func (h *handler) WebhookResponse(c echo.Context) error {
	// 讀取請求 Body
	body, err := io.ReadAll(c.Request().Body)

	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to read request body"})
	}
	defer c.Request().Body.Close()

	// 取得 LINE 簽名
	signature := c.Request().Header.Get("x-line-signature")
	if signature == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing signature"})
	}

	// 解碼簽名
	decodedSignature, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid signature encoding"})
	}

	// 計算 HMAC-SHA256
	hash := hmac.New(sha256.New, []byte("fdfa7381536ba9380268b0808733c344"))
	hash.Write(body)
	calculatedHash := hash.Sum(nil)

	// 比對簽名
	if !hmac.Equal(decodedSignature, calculatedHash) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
	}

	// 簽名驗證通過
	c.JSON(http.StatusOK, map[string]string{"message": "signature verified"})

	h.webhook.WebhookService(c, body)

	return c.JSON(http.StatusOK, map[string]string{"message": "success"})
}
