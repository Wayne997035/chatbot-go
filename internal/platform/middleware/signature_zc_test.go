package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// zcSendWebhookCtx 用指定 ctx 包住 request 送一次 webhook，回傳狀態碼.
func zcSendWebhookCtx(t *testing.T, ctx context.Context, mw echo.MiddlewareFunc, body []byte, handler echo.HandlerFunc) int {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Line-Signature", sign(body))
	rec := httptest.NewRecorder()
	if err := mw(handler)(e.NewContext(req, rec)); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec.Code
}

// [F30-88] TestForgetReplaySurvivesCancelledRequest 守住：回 503 時 request ctx 已取消，撤回防重放紀錄仍要成功（獨立期限），同 body 重送回 200 而不是 409.
func TestForgetReplaySurvivesCancelledRequest(t *testing.T) {
	useReplayCache(t, newLocalLineReplayCache())
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	mw := LineSignatureValidator("s3cret", client)
	body := []byte(`{"events":[{"webhookEventId":"01CANCELLED"}]}`)

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler := func(c echo.Context) error {
		if calls.Add(1) == 1 {
			cancel() // 模擬 request 結束後 ctx 已取消
			return c.NoContent(http.StatusServiceUnavailable)
		}
		return c.NoContent(http.StatusOK)
	}

	if got := zcSendWebhookCtx(t, ctx, mw, body, handler); got != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503", got)
	}
	if ctx.Err() == nil {
		t.Fatal("control: the first request's ctx must be canceled")
	}
	if got := zcSendWebhookCtx(t, context.Background(), mw, body, handler); got != http.StatusOK {
		t.Fatalf("resend status = %d, want 200 (record forgotten despite the canceled ctx), not 409", got)
	}
	if calls.Load() != 2 {
		t.Errorf("handler calls = %d, want 2", calls.Load())
	}
}
