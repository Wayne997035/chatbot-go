package httputil

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// [F34-78] 在 echo v4 上寫成、v4 上綠；升級 v5 後預期值不改：ErrorHandler 對各種錯誤的狀態碼與 body 逐位元不變.
// harness 區（建 echo、呼叫 ErrorHandler 的方式）可隨 API 改動，預期值不可.

// ---- harness ----

func newTestEcho() *echo.Echo {
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler
	return e
}

func addGet(e *echo.Echo, path string, h func(c echo.Context) error) {
	e.GET(path, h)
}

// ---- cases ----

func TestCharacterizeErrorHandlerBodiesByteForByte(t *testing.T) {
	e := newTestEcho()
	addGet(e, "/only-get", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	addGet(e, "/bad", func(echo.Context) error { return echo.NewHTTPError(http.StatusBadRequest, "bad thing") })
	addGet(e, "/teapot", func(echo.Context) error { return echo.NewHTTPError(http.StatusTeapot, "short and stout") })
	addGet(e, "/boom", func(echo.Context) error { return errors.New("mongo: connection refused") })
	addGet(e, "/wrapped", func(echo.Context) error {
		return errors.Join(errors.New("ctx"), echo.NewHTTPError(http.StatusBadRequest, "inner"))
	})

	cases := []struct {
		name   string
		method string
		target string
		code   int
		want   string
	}{
		{"404 not found", http.MethodGet, "/missing", 404, `{"status":{"code":"404","message":"Not Found"}}` + "\n"},
		{"405 method not allowed", http.MethodPost, "/only-get", 405, `{"status":{"code":"405","message":"Method Not Allowed"}}` + "\n"},
		{"400 HTTPError message", http.MethodGet, "/bad", 400, `{"status":{"code":"400","message":"bad thing"}}` + "\n"},
		{"418 HTTPError message", http.MethodGet, "/teapot", 418, `{"status":{"code":"418","message":"short and stout"}}` + "\n"},
		{"500 plain error hides the cause", http.MethodGet, "/boom", 500, `{"status":{"code":"5001","message":"internal server error"}}` + "\n"},
		{"wrapped HTTPError keeps its code", http.MethodGet, "/wrapped", 400, `{"status":{"code":"400","message":"inner"}}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), tc.method, tc.target, http.NoBody))
			if rec.Code != tc.code {
				t.Errorf("status = %d, want %d", rec.Code, tc.code)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

// 回應已 committed 之後的錯誤不再寫：狀態碼與 body 維持第一次送出的.
func TestCharacterizeErrorHandlerSkipsCommittedResponse(t *testing.T) {
	e := newTestEcho()
	addGet(e, "/late", func(c echo.Context) error {
		if err := c.String(http.StatusAccepted, "first"); err != nil {
			return err
		}
		return errors.New("late failure")
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/late", http.NoBody))
	if rec.Code != http.StatusAccepted || rec.Body.String() != "first" {
		t.Errorf("committed response changed: status %d body %q", rec.Code, rec.Body.String())
	}
}
