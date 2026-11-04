package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

const channelID = "1234567890"

// fakeLINE 模擬 LINE 的 ID token 驗證端點：token "good-<user>" 有效，"expired" 過期，"other-aud" 發給別的 channel.
func fakeLINE(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		token, aud := form.Get("id_token"), form.Get("client_id")
		exp := time.Now().Add(time.Hour).Unix()
		switch {
		case token == "down":
			w.WriteHeader(http.StatusBadGateway)
			return
		case token == "expired":
			exp = time.Now().Add(-time.Minute).Unix()
		case token == "other-aud":
			aud = "999"
		case !strings.HasPrefix(token, "good-"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request","error_description":"Invalid IdToken."}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iss": lineIssuer, "sub": strings.TrimPrefix(token, "good-"), "aud": aud, "exp": exp,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newVerifier(t *testing.T) (*Verifier, *atomic.Int32) {
	srv, calls := fakeLINE(t)
	v := NewVerifier(channelID, srv.Client(), nil)
	v.verifyURL = srv.URL
	return v, calls
}

func TestVerifierAcceptsValidTokenAndCaches(t *testing.T) {
	v, calls := newVerifier(t)
	for range 3 {
		uid, err := v.Verify(context.Background(), "good-U1")
		if err != nil || uid != "U1" {
			t.Fatalf("uid=%q err=%v", uid, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("LINE called %d times, want 1 (cached)", calls.Load())
	}
}

func TestVerifierRejectsBadTokens(t *testing.T) {
	v, _ := newVerifier(t)
	for _, token := range []string{"", "forged", "expired", "other-aud", strings.Repeat("x", maxTokenBytes+1)} {
		if _, err := v.Verify(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("token %.12q: err = %v", token, err)
		}
	}
	if _, err := v.Verify(context.Background(), "down"); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("LINE outage should be a non-auth error, got %v", err)
	}
}

// testClock 可推進的時鐘（Verifier.now 與本地快取共用）.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// clockLINE 以測試時鐘起點計算 exp 的假 LINE：token "exp5m" / "exp2h" 為起點後 5 分鐘 / 2 小時到期，
// "stale" 已過期，"down" 回 502.
func clockLINE(t *testing.T, clock *testClock) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	base := clock.Now() // exp 是絕對時間，推進時鐘後 LINE 會判定過期
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		exp := base
		switch form.Get("id_token") {
		case "exp5m":
			exp = exp.Add(5 * time.Minute)
		case "exp2h":
			exp = exp.Add(2 * time.Hour)
		case "stale":
			exp = exp.Add(-time.Minute)
		case "down":
			w.WriteHeader(http.StatusBadGateway)
			return
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iss": lineIssuer, "sub": "U1", "aud": form.Get("client_id"), "exp": exp.Unix(),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newClockVerifier(t *testing.T, cache *redis.Client) (*Verifier, *testClock, *atomic.Int32) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	srv, calls := clockLINE(t, clock)
	v := NewVerifier(channelID, srv.Client(), cache)
	v.verifyURL = srv.URL
	v.now = clock.Now
	return v, clock, calls
}

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（repo 沒有 miniredis）.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialTimeout:   50 * time.Millisecond,
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// [F24-45] 本地快取不得活得比 token 久，且 TTL 不超過 30 分鐘.
func TestVerifierLocalEntryNeverOutlivesToken(t *testing.T) {
	ctx := context.Background()

	t.Run("token expiry after cache is not served from cache", func(t *testing.T) {
		v, clock, calls := newClockVerifier(t, nil)
		if uid, err := v.Verify(ctx, "exp5m"); err != nil || uid != "U1" {
			t.Fatalf("uid=%q err=%v", uid, err)
		}
		clock.Advance(5*time.Minute + time.Second)
		if _, err := v.Verify(ctx, "exp5m"); err == nil {
			t.Error("expired token must not return cached user")
		}
		if calls.Load() != 2 {
			t.Errorf("LINE calls = %d, want 2", calls.Load())
		}
	})

	t.Run("whitebox entry past expires is ignored even if cache TTL not reached", func(t *testing.T) {
		v, clock, calls := newClockVerifier(t, nil)
		key := cacheKeyPrefix + hashToken("stale")
		v.local.SetTTL(key, cachedUser{userID: "U1", expires: clock.Now().Add(-time.Second)}, time.Hour)
		_, err := v.Verify(ctx, "stale")
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("err = %v, want ErrInvalidToken", err)
		}
		if calls.Load() != 1 {
			t.Errorf("LINE calls = %d, want 1 (stale entry must not be served)", calls.Load())
		}
	})

	t.Run("long-lived token cached at most 30 minutes", func(t *testing.T) {
		v, clock, calls := newClockVerifier(t, nil)
		if _, err := v.Verify(ctx, "exp2h"); err != nil {
			t.Fatal(err)
		}
		key := cacheKeyPrefix + hashToken("exp2h")
		entry, ok := v.local.Get(key)
		if !ok {
			t.Fatal("entry missing right after Verify")
		}
		if got := entry.expires.Sub(clock.Now()); got > 30*time.Minute {
			t.Errorf("stored expires is %v ahead, want <= 30m", got)
		}
		clock.Advance(31 * time.Minute)
		if _, ok := v.local.Get(key); ok {
			t.Error("local entry still present after 31m")
		}
		if _, err := v.Verify(ctx, "exp2h"); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Errorf("LINE calls = %d, want 2", calls.Load())
		}
	})
}

// [F24-45] Redis 出錯時 LIFF 驗證走本地快取；LINE 失敗不快取.
func TestVerifierLocalCacheWhenRedisErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("valid token cached locally", func(t *testing.T) {
		v, _, calls := newClockVerifier(t, deadRedis(t))
		for i := 0; i < 2; i++ {
			if uid, err := v.Verify(ctx, "exp5m"); err != nil || uid != "U1" {
				t.Fatalf("call %d: uid=%q err=%v", i, uid, err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("LINE calls = %d, want 1", calls.Load())
		}
	})

	t.Run("LINE outage returns non-auth error and is not cached", func(t *testing.T) {
		v, _, calls := newClockVerifier(t, deadRedis(t))
		for i := 0; i < 2; i++ {
			if _, err := v.Verify(ctx, "down"); err == nil || errors.Is(err, ErrInvalidToken) {
				t.Fatalf("call %d: err = %v, want non-auth error", i, err)
			}
		}
		if calls.Load() != 2 {
			t.Errorf("LINE calls = %d, want 2", calls.Load())
		}
	})

	t.Run("rejected token is not cached", func(t *testing.T) {
		v, _, calls := newClockVerifier(t, deadRedis(t))
		for i := 0; i < 2; i++ {
			if _, err := v.Verify(ctx, "stale"); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("call %d: err = %v", i, err)
			}
		}
		if calls.Load() != 2 {
			t.Errorf("LINE calls = %d, want 2", calls.Load())
		}
	})
}

// ---- API ----

type apiFixture struct {
	e        *echo.Echo
	meals    *storagetest.Meals
	workouts *storagetest.Workouts
	profiles *storagetest.Profiles
}

// now 固定在台灣 2026-11-02 20:00.
var fixedNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	v, _ := newVerifier(t)
	f := &apiFixture{e: echo.New(), meals: &storagetest.Meals{}, workouts: &storagetest.Workouts{}, profiles: storagetest.NewProfiles()}
	h := NewHandler(f.meals, f.workouts, f.profiles)
	h.now = func() time.Time { return fixedNow }
	h.Routes(f.e.Group("/api/v1/me", NoStore, Auth(v)))
	return f
}

func (f *apiFixture) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	return rec
}

func TestAPIRequiresVerifiedToken(t *testing.T) {
	f := newAPI(t)
	for _, token := range []string{"", "forged", "expired"} {
		if rec := f.do(http.MethodGet, "/api/v1/me/summary", token, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", token, rec.Code)
		}
	}
	if rec := f.do(http.MethodGet, "/api/v1/me/summary", "down", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("LINE outage: status %d", rec.Code)
	}
}

func TestSummaryUsesOnlyTheCallersData(t *testing.T) {
	f := newAPI(t)
	today := fixedNow
	yesterday := fixedNow.Add(-24 * time.Hour)
	f.meals.Items = []meal.Meal{
		{ID: "m1", UserID: "U1", EatenAt: today, Total: meal.Nutrients{Calories: 600, ProteinG: 30, CarbsG: 70, FatG: 20}},
		{ID: "m2", UserID: "U1", EatenAt: yesterday, Total: meal.Nutrients{Calories: 1500}},
		{ID: "m3", UserID: "U2", EatenAt: today, Total: meal.Nutrients{Calories: 9999}},
	}
	f.workouts.Items = []workout.Workout{{ID: "w1", UserID: "U1", Activity: "跑步", DistanceKM: 5, Calories: 343, DoneAt: today}}
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}

	rec := f.do(http.MethodGet, "/api/v1/me/summary?days=7", "good-U1", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	var s SummaryView
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Days) != 7 || s.Days[6].Date != "2026-11-02" || s.Days[5].Intake != 1500 {
		t.Errorf("days = %+v", s.Days)
	}
	if s.Today.Intake != 600 || s.Today.Burned != 343 || s.Today.Macros.ProteinG != 30 || s.Base != 1979 {
		t.Errorf("today = %+v base=%v", s.Today, s.Base)
	}
	if s.Days[6].RunKM != 5 || s.Totals.Calories != 2100 {
		t.Errorf("runKm=%v totals=%+v", s.Days[6].RunKM, s.Totals)
	}
}

func TestDayAndDeleteCannotTouchOtherUsers(t *testing.T) {
	f := newAPI(t)
	f.meals.Items = []meal.Meal{{ID: "mine", UserID: "U1", EatenAt: fixedNow}, {ID: "theirs", UserID: "U2", EatenAt: fixedNow}}
	f.workouts.Items = []workout.Workout{{ID: "w-theirs", UserID: "U2", DoneAt: fixedNow}}

	rec := f.do(http.MethodGet, "/api/v1/me/day?date=2026-11-02", "good-U1", "")
	var day DayRecords
	_ = json.Unmarshal(rec.Body.Bytes(), &day)
	if len(day.Meals) != 1 || day.Meals[0].ID != "mine" || strings.Contains(rec.Body.String(), "userId") {
		t.Fatalf("day = %s", rec.Body.String())
	}
	if rec := f.do(http.MethodDelete, "/api/v1/me/meals/theirs", "good-U1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting someone else's meal: status %d", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/v1/me/workouts/w-theirs", "good-U1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting someone else's workout: status %d", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/v1/me/meals/mine", "good-U1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("deleting own meal: status %d", rec.Code)
	}
	if len(f.meals.Items) != 1 || f.meals.Items[0].ID != "theirs" {
		t.Errorf("meals = %+v", f.meals.Items)
	}
	if rec := f.do(http.MethodGet, "/api/v1/me/day?date=11/02", "good-U1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad date: status %d", rec.Code)
	}
}

func TestProfileUpdateValidates(t *testing.T) {
	f := newAPI(t)
	if rec := f.do(http.MethodPut, "/api/v1/me/profile", "good-U1", `{"weightKg":999}`); rec.Code != http.StatusBadRequest {
		t.Errorf("absurd weight: status %d", rec.Code)
	}
	if rec := f.do(http.MethodPut, "/api/v1/me/profile", "good-U1", `{"weightKg":68,"heightCm":170,"age":30,"sex":"female"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if p, ok := f.profiles.Items["U1"]; !ok || p.WeightKG != 68 || p.Sex != profile.SexFemale {
		t.Errorf("profile = %+v", p)
	}
	rec := f.do(http.MethodGet, "/api/v1/me/profile", "good-U1", "")
	if !strings.Contains(rec.Body.String(), `"weightKg":68`) {
		t.Errorf("get profile: %s", rec.Body.String())
	}
}

func TestPageInjectsLIFFIDWithSecurityHeaders(t *testing.T) {
	page, static, err := Page("2001234567-AbCdEfGh")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.GET("/liff", page)
	e.GET("/liff/static/*", static)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/liff", http.NoBody))
	if !strings.Contains(rec.Body.String(), `data-liff-id="2001234567-AbCdEfGh"`) {
		t.Errorf("LIFF ID missing from page")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://static.line-scdn.net;") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP = %q", csp)
	}

	for _, file := range []string{"app.js", "app.css"} {
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/liff/static/"+file, http.NoBody))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: status %d", file, rec.Code)
		}
	}
}

func TestFrontendNeverUsesInnerHTML(t *testing.T) {
	js, err := webFS.ReadFile("web/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), "innerHTML") || strings.Contains(string(js), "insertAdjacentHTML") {
		t.Error("app.js must build DOM with textContent only (food names come from AI and users)")
	}
}

func TestScaleMealOnlyChangesTheCallersMeal(t *testing.T) {
	f := newAPI(t)
	items := []meal.FoodItem{{Name: "牛肉麵", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}}}
	f.meals.Items = []meal.Meal{
		{ID: "mine", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients},
		{ID: "theirs", UserID: "U2", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients},
	}

	rec := f.do(http.MethodPatch, "/api/v1/me/meals/mine", "good-U1", `{"scale":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got MealView
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Total.Calories != 325 || got.Scale != 0.5 || strings.Contains(rec.Body.String(), "userId") || strings.Contains(rec.Body.String(), "base") {
		t.Errorf("response = %s", rec.Body.String())
	}
	if f.meals.Items[0].Total.Calories != 325 {
		t.Errorf("stored = %+v", f.meals.Items[0].Total)
	}

	if rec := f.do(http.MethodPatch, "/api/v1/me/meals/theirs", "good-U1", `{"scale":2}`); rec.Code != http.StatusNotFound {
		t.Errorf("another user's meal: status %d", rec.Code)
	}
	if f.meals.Items[1].Total.Calories != 650 {
		t.Error("another user's meal must not change")
	}
	for _, body := range []string{`{"scale":0}`, `{"scale":10}`, `{"scale":"half"}`, `not json`} {
		if rec := f.do(http.MethodPatch, "/api/v1/me/meals/mine", "good-U1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d", body, rec.Code)
		}
	}
	if rec := f.do(http.MethodPatch, "/api/v1/me/meals/mine", "", `{"scale":1}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d", rec.Code)
	}
}

// 在 LIFF 改份量的同時，聊天那邊剛加了品項：在最新資料上改份量，加的品項不會不見.
func TestScaleMealKeepsChatEditsMadeMeanwhile(t *testing.T) {
	f := newAPI(t)
	items := []meal.FoodItem{{Name: "牛肉麵", Nutrients: meal.Nutrients{Calories: 650}}}
	f.meals.Items = []meal.Meal{{ID: "mine", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients}}
	f.meals.BeforeUpdate = func(m *storagetest.Meals) {
		m.BeforeUpdate = nil
		stored := &m.Items[0]
		stored.Items = append(stored.Items, meal.FoodItem{Name: "滷蛋", Nutrients: meal.Nutrients{Calories: 70}})
		stored.Rebase()
		stored.Version++
	}

	rec := f.do(http.MethodPatch, "/api/v1/me/meals/mine", "good-U1", `{"scale":0.5}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := f.meals.Items[0]
	if len(got.Items) != 2 || got.Total.Calories != 360 || got.Scale != 0.5 {
		t.Errorf("both items are halved: %+v", got)
	}
	if f.meals.Updates != 2 {
		t.Errorf("updates = %d, want a conflict then a retry", f.meals.Updates)
	}
}

// 一直被別處修改時回 409，前端會重新載入；不會蓋掉別處的修改.
func TestScaleMealReportsAConflictWhenTheMealKeepsChanging(t *testing.T) {
	f := newAPI(t)
	items := []meal.FoodItem{{Name: "牛肉麵", Nutrients: meal.Nutrients{Calories: 650}}}
	f.meals.Items = []meal.Meal{{ID: "mine", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients}}
	f.meals.BeforeUpdate = func(m *storagetest.Meals) { m.Items[0].Version++ }

	rec := f.do(http.MethodPatch, "/api/v1/me/meals/mine", "good-U1", `{"scale":0.5}`)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"4009"`) {
		t.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	if f.meals.Items[0].Total.Calories != 650 || f.meals.Updates != 3 {
		t.Errorf("stored = %+v after %d updates", f.meals.Items[0].Total, f.meals.Updates)
	}
}
