package server

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/health"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/user"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	userstore "chatbot-go/internal/storage/database/user"
)

const statsTestToken = "admin-token-for-route-test"

// statsFakeStore 固定回 3 位使用者，並計數被呼叫幾次（POST 等非 GET 方法不得觸發任何讀寫）.
type statsFakeStore struct{ calls atomic.Int64 }

func (f *statsFakeStore) CountUsers(context.Context) (int64, error) {
	f.calls.Add(1)
	return 3, nil
}

func (f *statsFakeStore) CountUsersCreatedSince(context.Context, time.Time) (int64, error) {
	f.calls.Add(1)
	return 1, nil
}

func (f *statsFakeStore) ListUsersPage(context.Context, int, int) ([]userstore.User, error) {
	f.calls.Add(1)
	return []userstore.User{{ID: "U0123456789abcdef0123456789abcdef", CreateTime: time.Now()}}, nil
}

func (f *statsFakeStore) CountEnabledByType(context.Context, string) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}

func (f *statsFakeStore) CountDistinctEnabledUsers(context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}

func (f *statsFakeStore) CountWeatherWarningRegions(context.Context) ([]alertsub.RegionCount, error) {
	f.calls.Add(1)
	return nil, nil
}

func (f *statsFakeStore) FindSubscriptionsByUserIDs(context.Context, []string) ([]alertsub.AlertSubscription, error) {
	f.calls.Add(1)
	return nil, nil
}

func (f *statsFakeStore) CountFilledProfiles(context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}

func statsRoutes(t *testing.T, adminToken string, store user.StatsStore) *echo.Echo {
	t.Helper()
	e, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{
		Health:  health.NewHealthHandler(),
		Webhook: webhook.NewWebhookHandler(nil, nil, nil, nil, nil),
		User:    user.NewUserHandler(store, nil),
		Weather: weather.NewWeatherHandler(nil, &config.Config{}),
		Alert:   alert.NewAlertHandler(nil),
	}
	Routes(e, h, Security{ChannelSecret: "channel-secret", AdminToken: adminToken})
	return e
}

func statsDo(e *echo.Echo, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// F30-70 token 驗證：正確（兩種寫法）200；缺 / 錯 401 且 body 不含統計資料；非 GET 方法不得 2xx 也不得觸發 store.
func TestStatsAuth(t *testing.T) {
	store := &statsFakeStore{}
	e := statsRoutes(t, statsTestToken, store)

	for name, headers := range map[string]map[string]string{
		"X-Admin-Token": {"X-Admin-Token": statsTestToken},
		"Bearer":        {"Authorization": "Bearer " + statsTestToken},
	} {
		rec := statsDo(e, http.MethodGet, "/api/v1/stats", headers)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
			!strings.Contains(rec.Body.String(), `"total":3`) {
			t.Errorf("%s: code=%d ct=%q body=%s", name, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	callsAfterOK := store.calls.Load()
	for name, headers := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {"X-Admin-Token": "wrong-token"},
	} {
		rec := statsDo(e, http.MethodGet, "/api/v1/stats", headers)
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), `"users"`) {
			t.Errorf("%s: code=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := statsDo(e, m, "/api/v1/stats", map[string]string{"X-Admin-Token": statsTestToken}); rec.Code < http.StatusBadRequest {
			t.Errorf("%s /api/v1/stats with a valid token = %d, want >= 400", m, rec.Code)
		}
	}
	if got := store.calls.Load(); got != callsAfterOK {
		t.Errorf("store was touched by rejected requests: %d -> %d", callsAfterOK, got)
	}
}

// F30-70 伺服器沒設 ADMIN_TOKEN → 有 token、沒 token 都是 503.
func TestStatsRouteNeedsAdminToken(t *testing.T) {
	e := statsRoutes(t, "", &statsFakeStore{})
	for name, headers := range map[string]map[string]string{
		"with token": {"X-Admin-Token": statsTestToken},
		"no token":   nil,
	} {
		if rec := statsDo(e, http.MethodGet, "/api/v1/stats", headers); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: code=%d, want 503", name, rec.Code)
		}
	}
}

// F30-70 HEAD 打統計 API 回精確 404（不是規格原寫的 405）且不讀 store：不得 2xx、不得觸發 store.
// 實測偏差：規格寫 405，但 Echo v4.16.3 對有中介層的 group 註冊 RouteNotFound("/*")（group.go:31-32），
// 方法不符時走該 catch-all，實際是 404；兩者都不是 2xx，守的需求（HEAD 不能拿到統計）不變.
func TestStatsHeadIsNotServed(t *testing.T) {
	store := &statsFakeStore{}
	e := statsRoutes(t, statsTestToken, store)
	rec := statsDo(e, http.MethodHead, "/api/v1/stats", map[string]string{"X-Admin-Token": statsTestToken})
	if rec.Code != http.StatusNotFound {
		t.Errorf("HEAD /api/v1/stats = %d, want 404 (Echo catch-all; must never be 2xx)", rec.Code)
	}
	if store.calls.Load() != 0 {
		t.Errorf("HEAD touched the store %d times", store.calls.Load())
	}
}

// F30-77 舊端點已移除：有 token 404、沒 token 401（token 驗證套在 group 層）；GetAllUsers 符號不存在.
func TestUsersEndpointRemoved(t *testing.T) {
	e := statsRoutes(t, statsTestToken, &statsFakeStore{})
	rec := statsDo(e, http.MethodGet, "/api/v1/users", map[string]string{"X-Admin-Token": statsTestToken})
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "U0123456789abcdef") {
		t.Errorf("GET /api/v1/users with token = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := statsDo(e, http.MethodGet, "/api/v1/users", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/users without token = %d, want 401", rec.Code)
	}
	if _, ok := reflect.TypeFor[*user.UserHandler]().MethodByName("GetAllUsers"); ok {
		t.Error("UserHandler.GetAllUsers must not exist")
	}
}
