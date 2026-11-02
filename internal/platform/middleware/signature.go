package middleware

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

const (
	maxLineWebhookBodyBytes    int64 = 1 << 20
	lineWebhookReplayTTL             = 5 * time.Minute
	lineWebhookReplayKeyPrefix       = "line:webhook:replay:"
)

var (
	errLineWebhookBodyTooLarge = errors.New("line webhook body exceeds size limit")
	errLineWebhookReplay       = errors.New("line webhook replay detected")
	lineReplayCache            = newLocalLineReplayCache()
)

// LineSignatureValidator LINE Webhook 簽名驗證 middleware；replayClient 可為 nil（只用記憶體防重放）.
func LineSignatureValidator(channelSecret string, replayClient *redis.Client) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			signature := c.Request().Header.Get("X-Line-Signature")
			if signature == "" {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Missing X-Line-Signature header"))
			}

			body, err := readLineWebhookBody(c.Request().Body)
			if err != nil {
				if errors.Is(err, errLineWebhookBodyTooLarge) {
					return c.JSON(http.StatusRequestEntityTooLarge, httputil.ErrorWithCode(
						httputil.ErrorCodeInvalidParameter, "Request body too large"))
				}
				return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidParameter, "Failed to read body"))
			}
			// 回寫 body 供 handler 讀取
			c.Request().Body = io.NopCloser(bytes.NewReader(body))

			if !validateHMAC(body, signature, channelSecret) {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Invalid signature"))
			}
			if errors.Is(recordLineWebhookBody(c.Request().Context(), replayClient, body), errLineWebhookReplay) {
				return c.JSON(http.StatusConflict, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidParameter, "Duplicate webhook request"))
			}

			return next(c)
		}
	}
}

func readLineWebhookBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxLineWebhookBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxLineWebhookBodyBytes {
		return nil, errLineWebhookBodyTooLarge
	}
	return body, nil
}

func validateHMAC(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// recordLineWebhookBody 記錄這個 webhook 內容；5 分鐘內重複收到回傳 errLineWebhookReplay.
// Redis 不能用時改用記憶體，絕不因此拒收：LINE 預設不重送，拒收就等於訊息遺失.
func recordLineWebhookBody(ctx context.Context, client *redis.Client, body []byte) error {
	key := lineWebhookReplayKey(body)
	if client != nil {
		ok, err := client.SetNX(ctx, key, "1", lineWebhookReplayTTL).Result()
		if err == nil {
			if !ok {
				return errLineWebhookReplay
			}
			return nil
		}
		slog.Warn("webhook replay check falling back to memory", "error", err)
	}
	if !lineReplayCache.record(key, time.Now().Add(lineWebhookReplayTTL)) {
		return errLineWebhookReplay
	}
	return nil
}

func lineWebhookReplayKey(body []byte) string {
	sum := sha256.Sum256(body)
	return lineWebhookReplayKeyPrefix + hex.EncodeToString(sum[:])
}

type localLineReplayCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func newLocalLineReplayCache() *localLineReplayCache {
	return &localLineReplayCache{entries: make(map[string]time.Time)}
}

func (c *localLineReplayCache) record(key string, expiresAt time.Time) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	for existingKey, existingExpiry := range c.entries {
		if now.After(existingExpiry) {
			delete(c.entries, existingKey)
		}
	}

	if existingExpiry, ok := c.entries[key]; ok && now.Before(existingExpiry) {
		return false
	}
	c.entries[key] = expiresAt
	return true
}
