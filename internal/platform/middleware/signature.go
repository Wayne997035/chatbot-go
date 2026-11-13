package middleware

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

const (
	maxLineWebhookBodyBytes    int64 = 1 << 20
	lineWebhookReplayTTL             = 5 * time.Minute
	lineWebhookReplayKeyPrefix       = "line:webhook:replay:"
	// lineReplayWarnAbove 本地防重放筆數超過這個數字只發 Warn，不淘汰、不拒寫（被丟 = 放行重放）.
	// 算式：5 分鐘內相異 webhook ≈ 請求率 r=100 req/s × 300 s = 30_000，取 >3x 餘裕 = 100_000
	// （r 為估算值，未實測）；每筆約 148.5 B（G0 實測），100_000 筆約 15 MB.
	lineReplayWarnAbove = 100_000
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
					httputil.ErrorCodeInvalidSignature, "Missing X-Line-Signature header",
				))
			}

			body, err := readLineWebhookBody(c.Request().Body)
			if err != nil {
				if errors.Is(err, errLineWebhookBodyTooLarge) {
					return c.JSON(http.StatusRequestEntityTooLarge, httputil.ErrorWithCode(
						httputil.ErrorCodeInvalidParameter, "Request body too large",
					))
				}
				return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidParameter, "Failed to read body",
				))
			}
			// 回寫 body 供 handler 讀取
			c.Request().Body = io.NopCloser(bytes.NewReader(body))

			if !validateHMAC(body, signature, channelSecret) {
				return c.JSON(http.StatusUnauthorized, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidSignature, "Invalid signature",
				))
			}
			if errors.Is(recordLineWebhookBody(c.Request().Context(), replayClient, body), errLineWebhookReplay) {
				return c.JSON(http.StatusConflict, httputil.ErrorWithCode(
					httputil.ErrorCodeInvalidParameter, "Duplicate webhook request",
				))
			}

			err = next(c)
			// [F30-67] 503 = handler 還沒接手（忙碌），LINE 重送同一個 body 時不能被防重放擋掉；撤回這次的紀錄.
			// 只撤回 503：其他狀態碼（含 500）可能已處理過一部分，重送會重複執行.
			// 紀錄點仍在 handler 之前，所以處理期間的並發重放照樣 409.
			if c.Response().Status == http.StatusServiceUnavailable {
				forgetLineWebhookBody(c.Request().Context(), replayClient, body)
			}
			return err
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
	if !lineReplayCache.record(key) {
		return errLineWebhookReplay
	}
	return nil
}

// forgetLineWebhookBodyTimeout 撤回防重放紀錄的 Redis 逾時.
const forgetLineWebhookBodyTimeout = 500 * time.Millisecond

// forgetLineWebhookBody [F30-67] 撤回 recordLineWebhookBody 的紀錄（Redis 與本地快取都清）；失敗只 Warn.
func forgetLineWebhookBody(ctx context.Context, client *redis.Client, body []byte) {
	key := lineWebhookReplayKey(body)
	if client != nil {
		// request 結束後 ctx 可能已取消，撤回要用獨立的逾時.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetLineWebhookBodyTimeout)
		defer cancel()
		if err := client.Del(dctx, key).Err(); err != nil {
			slog.Warn("webhook replay record not forgotten", "error", redact.Error(err))
		}
	}
	lineReplayCache.forget(key)
}

func lineWebhookReplayKey(body []byte) string {
	sum := sha256.Sum256(body)
	return lineWebhookReplayKeyPrefix + hex.EncodeToString(sum[:])
}

// [F24-53] localLineReplayCache 本地防重放：NoEvict + 原子 SetIfAbsent，不掃全表、不淘汰.
type localLineReplayCache struct {
	c *localcache.Cache[struct{}]
}

func newLocalLineReplayCache() *localLineReplayCache {
	return &localLineReplayCache{c: localcache.NewNoEvict[struct{}]("line-webhook-replay", lineWebhookReplayTTL, lineReplayWarnAbove)}
}

// newLocalLineReplayCacheWithClock 測試用：注入時鐘.
func newLocalLineReplayCacheWithClock(now func() time.Time) *localLineReplayCache {
	return &localLineReplayCache{
		c: localcache.NewNoEvictWithClock[struct{}]("line-webhook-replay", lineWebhookReplayTTL, lineReplayWarnAbove, now),
	}
}

// forget 撤回一筆紀錄.
func (c *localLineReplayCache) forget(key string) {
	c.c.Delete(key)
}

// record 回傳 true = 第一次看到.
func (c *localLineReplayCache) record(key string) bool {
	return c.c.SetIfAbsent(key, struct{}{}, lineWebhookReplayTTL)
}
