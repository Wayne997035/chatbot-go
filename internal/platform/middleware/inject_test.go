package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func serveThrough(mw echo.MiddlewareFunc, req *http.Request) (status int, reached bool) {
	e := echo.New()
	rec := httptest.NewRecorder()
	h := mw(func(c echo.Context) error {
		reached = true
		return c.NoContent(http.StatusOK)
	})
	_ = h(e.NewContext(req, rec))
	return rec.Code, reached
}

// 管理 token 由組裝層傳入：沒設定時一律 503，錯的 401，對的才放行.
func TestAdminTokenValidatorUsesTheGivenToken(t *testing.T) {
	newReq := func(token string) *http.Request {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req
	}
	cases := []struct {
		configured, sent string
		status           int
		reached          bool
	}{
		{"", "anything", http.StatusServiceUnavailable, false},
		{"right-token-value", "", http.StatusUnauthorized, false},
		{"right-token-value", "wrong-token-value", http.StatusUnauthorized, false},
		{"right-token-value", "right-token-value", http.StatusOK, true},
	}
	for _, tc := range cases {
		status, reached := serveThrough(AdminTokenValidator(tc.configured), newReq(tc.sent))
		if status != tc.status || reached != tc.reached {
			t.Errorf("configured=%q sent=%q: status %d reached %v", tc.configured, tc.sent, status, reached)
		}
	}
}

// LINE 簽章用組裝層傳入的 channel secret 驗證.
func TestLineSignatureValidatorUsesTheGivenSecret(t *testing.T) {
	body := `{"events":[],"destination":"Ubot-inject-test"}`
	sign := func(secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	newReq := func(sig string) *http.Request {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("X-Line-Signature", sig)
		return req
	}
	mw := LineSignatureValidator("channel-secret-a", nil)
	if status, reached := serveThrough(mw, newReq(sign("channel-secret-b"))); status != http.StatusUnauthorized || reached {
		t.Errorf("signed with another secret: %d %v", status, reached)
	}
	if status, reached := serveThrough(mw, newReq(sign("channel-secret-a"))); status != http.StatusOK || !reached {
		t.Errorf("valid signature: %d %v", status, reached)
	}
}
