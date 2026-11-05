package server

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/liff"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/health"
	"chatbot-go/internal/user"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func testRoutes(t *testing.T, withLIFF bool) *echo.Echo {
	t.Helper()
	e, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{
		Health:  health.NewHealthHandler(),
		Webhook: webhook.NewWebhookHandler(nil, nil, nil, nil, nil),
		User:    user.NewUserHandler(nil),
		Weather: weather.NewWeatherHandler(nil, &config.Config{}),
		Alert:   alert.NewAlertHandler(nil),
	}
	if withLIFF {
		page, static, err := liff.Page("1234567890-AbCdEfGh")
		if err != nil {
			t.Fatal(err)
		}
		h.LIFF = &LIFF{Page: page, Static: static, API: liff.NewHandler(nil, nil, nil), Verifier: liff.NewVerifier("1234567890", nil, nil)}
	}
	Routes(e, h, Security{ChannelSecret: "channel-secret", AdminToken: "admin-token-for-route-test"})
	return e
}

func do(e *echo.Echo, method, path string) int {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("{}")))
	return rec.Code
}

// 路由表：管理 API 都要 token、webhook 要簽章、LIFF 沒設定時不存在.
func TestRoutesAreProtected(t *testing.T) {
	e := testRoutes(t, false)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/openDataUpdate"},
		{http.MethodGet, "/api/v1/alerts/subscriptions/U1"},
		{http.MethodPost, "/api/v1/alerts/subscriptions"},
		{http.MethodDelete, "/api/v1/alerts/subscriptions/U1/earthquake_tsunami"},
	} {
		if code := do(e, r.method, r.path); code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d", r.method, r.path, code)
		}
	}
	if code := do(e, http.MethodPost, "/api/v1/webhooks"); code != http.StatusUnauthorized {
		t.Errorf("webhook without signature: %d", code)
	}
	if code := do(e, http.MethodGet, "/health"); code != http.StatusOK {
		t.Errorf("/health: %d", code)
	}
	if code := do(e, http.MethodGet, "/ready"); code != http.StatusOK {
		t.Errorf("/ready with no checks: %d", code)
	}
	if code := do(e, http.MethodGet, "/liff"); code != http.StatusNotFound {
		t.Errorf("/liff must not exist when LIFF is off: %d", code)
	}
}

func TestLIFFRoutesWhenEnabled(t *testing.T) {
	e := testRoutes(t, true)
	if code := do(e, http.MethodGet, "/liff"); code != http.StatusOK {
		t.Errorf("/liff: %d", code)
	}
	if code := do(e, http.MethodGet, "/api/v1/me/summary"); code != http.StatusUnauthorized {
		t.Errorf("/me without a LINE ID token: %d", code)
	}
}

// [F24-81] 監控用 HEAD 打 /health 也回 200 並觸發 Redis 保活；HEAD /ready 也回 200.
func TestHealthAcceptsHEADAndKeepsAlive(t *testing.T) {
	e, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan struct{}, 1)
	h := &Handlers{
		Health: health.NewHealthHandler().WithKeepAlive(func(context.Context) error {
			wrote <- struct{}{}
			return nil
		}, nil),
		Webhook: webhook.NewWebhookHandler(nil, nil, nil, nil, nil),
		User:    user.NewUserHandler(nil),
		Weather: weather.NewWeatherHandler(nil, &config.Config{}),
		Alert:   alert.NewAlertHandler(nil),
	}
	Routes(e, h, Security{ChannelSecret: "channel-secret", AdminToken: "admin-token-for-route-test"})

	if code := do(e, http.MethodHead, "/health"); code != http.StatusOK {
		t.Errorf("HEAD /health = %d, want 200", code)
	}
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
		t.Error("HEAD /health did not trigger the keep-alive write")
	}
	if code := do(e, http.MethodHead, "/ready"); code != http.StatusOK {
		t.Errorf("HEAD /ready = %d, want 200", code)
	}
}
