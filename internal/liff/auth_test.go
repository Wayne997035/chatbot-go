package liff

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// authRoundTripper 把每個請求交給 fn.
type authRoundTripper func(*http.Request) (*http.Response, error)

func (f authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// authFailingBody 讀取時回含 email 與位址的錯誤（verifier.go 的 read verify response 保留原文）.
type authFailingBody struct{}

func (authFailingBody) Read([]byte) (int, error) {
	return 0, errors.New("read failed for a@b.test at 10.0.0.5:443")
}
func (authFailingBody) Close() error { return nil }

// TestAuthLogsRedactVerifierError [F303-09] 驗證器回非 ErrInvalidToken 的錯誤時，回 503、log 有訊息且 error 欄經遮蔽，不含 email 與位址原文.
func TestAuthLogsRedactVerifierError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := &http.Client{Transport: authRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: authFailingBody{}, Header: http.Header{}}, nil
	})}
	v := NewVerifier("chan-1", client, nil)
	v.verifyURL = "http://line.invalid/verify"

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := Auth(v)(func(echo.Context) error { return c.NoContent(http.StatusOK) })
	if err := h(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	got := buf.String()
	if !strings.Contains(got, "LIFF token verification unavailable") {
		t.Fatalf("log missing message: %s", got)
	}
	for _, raw := range []string{"a@b.test", "10.0.0.5"} {
		if strings.Contains(got, raw) {
			t.Errorf("log leaks %q: %s", raw, got)
		}
	}
	for _, mark := range []string{"[email]", "[addr]"} {
		if !strings.Contains(got, mark) {
			t.Errorf("log missing redaction mark %q: %s", mark, got)
		}
	}
}
