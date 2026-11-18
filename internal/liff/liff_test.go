package liff

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
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

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（連線錯誤）.
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

// frontendSources 讀出 app.js 與 index.html（字串冒煙測試用；畫面行為由 PR 審查的無頭瀏覽器驗）.
func frontendSources(t *testing.T) (js, html string) {
	t.Helper()
	j, err := webFS.ReadFile("web/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(j), string(h)
}

// jsFuncBody 取出 `  function name(` 到下一個頂層 `\n  function ` / `\n  var ` / `\n  // ----` 之前的文字.
func jsFuncBody(t *testing.T, js, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(js, "  function "+name+"(")
	if !ok {
		t.Fatalf("function %s not found in app.js", name)
	}
	end := len(rest)
	for _, stop := range []string{"\n  function ", "\n  var ", "\n  // ----"} {
		if i := strings.Index(rest, stop); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

func mustContainAll(t *testing.T, where, src string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(src, w) {
			t.Errorf("%s is missing %q", where, w)
		}
	}
}

func TestCSPKeepsLineAvatarSourceAndStaysStrict(t *testing.T) {
	// [F30-21] 頭像網域要在 CSP img-src 內；不得放寬 script / style.
	if !strings.Contains(contentSecurityPolicy, "img-src 'self' data: https://*.line-scdn.net") {
		t.Errorf("img-src lost https://*.line-scdn.net: %q", contentSecurityPolicy)
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(contentSecurityPolicy, bad) {
			t.Errorf("CSP contains %s", bad)
		}
	}
}

// TestFrontendConsentGateBlocksAPI [F30-20] api() 在送出任何請求之前先檢查同意；告知內容齊全；同意值讀寫失敗都視為未同意.
func TestFrontendConsentGateBlocksAPI(t *testing.T) {
	js, html := frontendSources(t)
	api := jsFuncBody(t, js, "api")
	gate := strings.Index(api, "!state.consented")
	fetch := strings.Index(api, "fetch(")
	if gate < 0 || fetch < 0 || gate > fetch {
		t.Fatalf("api() must reject on !state.consented before fetch(): gate=%d fetch=%d", gate, fetch)
	}
	if !strings.Contains(api[gate:fetch], "no-consent") {
		t.Error("api() must reject with no-consent when not consented")
	}
	mustContainAll(t, "app.js", js,
		`var PRIVACY_VERSION = "`, "localStorage.getItem(CONSENT_KEY) === PRIVACY_VERSION",
		"無法儲存同意狀態，請稍後再試", "liff.closeWindow()")
	// 讀取例外 → false（未同意）；寫入後讀回不等於版本 → false（不放行）.
	if has := jsFuncBody(t, js, "hasConsent"); !strings.Contains(has, "catch") || !strings.Contains(has, "return false") {
		t.Errorf("hasConsent must treat storage errors as not consented: %s", has)
	}
	if save := jsFuncBody(t, js, "saveConsent"); !strings.Contains(save, "catch") || !strings.Contains(save, "return false") {
		t.Errorf("saveConsent must report failure when storage throws: %s", save)
	}
	// 不同意：不寫入同意值（decline 處理器不得呼叫 saveConsent / api）.
	_, decline, _ := strings.Cut(js, `getElementById("consent-decline")`)
	decline, _, _ = strings.Cut(decline, "});")
	if strings.Contains(decline, "saveConsent") || strings.Contains(decline, "api(") || strings.Contains(decline, "state.consented = true") {
		t.Errorf("decline handler must not consent or call the API: %s", decline)
	}

	// 告知內容（個資法第 8 條六項與 user 提供的事實）.
	mustContainAll(t, "index.html", html,
		"1. 經營者", "2. 蒐集目的", "3. 蒐集的個人資料類別", "4. 利用期間、地區、對象與方式", "5. 你的權利與行使方式", "6. 不提供的影響",
		"韋恩紅茶", "LINE 顯示名稱", "頭像", "身體資料", "飲食與運動紀錄", "餐點照片與文字",
		"保存至你刪除為止", "對話狀態 5 分鐘", "30 分鐘", "7 天", "美國及台灣", "刪除我的資料", "LINE 聊天室", "聊天室內的功能照常使用",
		"同意並繼續", "不同意", "range .AIProviders")
	// 前端不得寫死任何供應商名稱（名單由伺服器渲染）.
	for _, name := range []string{"Gemini", "Groq", "NVIDIA", "Claude", "Anthropic"} {
		if strings.Contains(js, name) || strings.Contains(html, name) {
			t.Errorf("provider name %q must not be hard-coded in app.js / index.html", name)
		}
	}
	// 同意前 app 區塊與資料都不顯示；使用者可從設定頁再看說明.
	mustContainAll(t, "index.html", html, `<div id="app" hidden>`, `<section id="gate" class="gate" hidden>`, `id="privacy-open"`)
}

// TestFrontendMemberAndDeleteAccount [F30-21] 會員資料與刪除帳號：不用 innerHTML、頭像限 https、不顯示也不蒐集 email、成功後才清同意.
func TestFrontendMemberAndDeleteAccount(t *testing.T) {
	js, html := frontendSources(t)
	render := jsFuncBody(t, js, "renderAccount")
	mustContainAll(t, "renderAccount", render, `isHttpsUrl(a.pictureUrl)`, `.textContent = name`, "avatar-initial", `"error"`)
	if strings.Contains(render, "innerHTML") {
		t.Error("renderAccount must use textContent only")
	}
	// [F31-15] 不收 email：畫面、告知與前端程式都不得再出現 email 的蒐集或顯示.
	for name, src := range map[string]string{"app.js": js, "index.html": html} {
		if strings.Contains(strings.ToLower(src), "email") {
			t.Errorf("[F31-15] %s must not mention email any more", name)
		}
	}
	// [F31-33] email 欄移除後不留死樣式.
	css, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), ".account-text span") {
		t.Error("[F31-33] app.css must not keep the dead .account-text span rule")
	}
	if !strings.Contains(jsFuncBody(t, js, "isHttpsUrl"), `/^https:\/\//i`) {
		t.Error("avatar URL must be restricted to https")
	}
	mustContainAll(t, "index.html", html, "刪除我的帳號資料", `id="delete-cancel"`, `id="delete-ok"`, "確定刪除", "無法復原", "LINE 帳號本身不受影響")
	// 預設焦點在「取消」；刪除確認不得用裸 confirm().
	mustContainAll(t, "app.js", js, `openDialog(deleteBox, document.getElementById("delete-cancel")`, `api("/account", { method: "DELETE" })`, "部分資料刪除失敗，請稍後再試一次", "刪除失敗，請稍後再試")
	// 成功後：先清本機同意值，再清畫面；失敗分支（.catch）不得呼叫 clearConsent.
	after := jsFuncBody(t, js, "afterAccountDeleted")
	ci, ri := strings.Index(after, "clearConsent()"), strings.Index(after, "resetDayCard(")
	if ci < 0 || ri < 0 || ci > ri || !strings.Contains(after, "state.consented = false") || !strings.Contains(after, `showGate({ deleted: true })`) {
		t.Errorf("afterAccountDeleted must clear consent first, then the screen, then show the gate: %s", after)
	}
	_, onDelete, _ := strings.Cut(js, `api("/account", { method: "DELETE" })`)
	_, failure, _ := strings.Cut(onDelete, ".catch(")
	failure, _, _ = strings.Cut(failure, "});")
	if strings.Contains(failure, "clearConsent") || strings.Contains(failure, "afterAccountDeleted") {
		t.Errorf("delete failure branch must keep consent and screen: %s", failure)
	}
	if !strings.Contains(html, "資料已刪除") {
		t.Error("gate must show 資料已刪除 after a successful delete")
	}
}

// TestFrontendNotificationsSection [F30-22] 通知區：兩列開關、關閉整類、改地區 R7 整份取代、取消零請求、失敗保留選項.
func TestFrontendNotificationsSection(t *testing.T) {
	js, html := frontendSources(t)
	// 標題列的深淺色開關 [F30-26] 也是 role=switch，不算通知列.
	if n := strings.Count(html, `role="switch"`) - strings.Count(html, `id="theme-toggle"`); n != 2 {
		t.Errorf("want exactly 2 role=switch rows (earthquake + weather), got %d", n)
	}
	mustContainAll(t, "index.html", html, "目前沒有開啟任何通知。", "地震海嘯警告", "天氣特報")
	mustContainAll(t, "app.js", js,
		`"/alerts/earthquake_tsunami"`, `"/alerts/weather_warning"`, `{ method: "PUT", body: JSON.stringify({ regions: regions }) }`, `{ method: "DELETE" }`,
		"關閉失敗，請稍後再試", "開啟失敗，請稍後再試", "「開啟天氣特報」「開啟地震海嘯警告」",
		"你的資料已經刪除，這筆沒有開啟", "部分資料刪除失敗，請稍後再試一次", "地區清單讀取失敗", "通知讀取失敗，請稍後再試",
		`api("/regions")`, "修改地區", "儲存地區", "開啟通知")
	// 地區字串一律 textContent；不轉「臺」、不自備縣市清單.
	alerts := jsFuncBody(t, js, "renderAlerts")
	if strings.Contains(alerts, "innerHTML") || !strings.Contains(alerts, `el("span", "region-name", r)`) {
		t.Errorf("renderAlerts must render regions via textContent: %s", alerts)
	}
	if strings.Contains(js, "臺") {
		t.Error("app.js must not convert or hard-code 臺: region strings come from /regions as-is")
	}
	// 成功後以伺服器回的 regions 為準.
	submit := jsFuncBody(t, js, "submitPicker")
	mustContainAll(t, "submitPicker", submit, "item.regions", "http-410")
	// 取消：closePicker 不發請求，且丟棄已選項目.
	closeFn := jsFuncBody(t, js, "closePicker")
	if strings.Contains(closeFn, "api(") || !strings.Contains(closeFn, "picker.entries = []") {
		t.Errorf("closePicker must not call the API and must drop selections: %s", closeFn)
	}
	// 已開啟改地區不先關再開：openPicker 流程中沒有 DELETE.
	if strings.Contains(jsFuncBody(t, js, "openPicker")+submit, `"DELETE"`) {
		t.Error("editing regions must be one PUT, never a DELETE first")
	}
}

// netBand [F34-57] 獨立的 300（前端不得寫淨值門檻）；3000 不命中.
var netBand = regexp.MustCompile(`\b300\b`)

// TestFrontendCalendarTab [F30-23] 月曆：週日為第一欄、grade 白名單對應 class、前端不判級.
func TestFrontendCalendarTab(t *testing.T) {
	js, html := frontendSources(t)
	mustContainAll(t, "index.html", html, `data-tab="calendar"`, `id="cal-grid"`, `id="month-prev"`, `id="month-next"`)
	mustContainAll(t, "app.js", js, `"/month?month="`, "getUTCDay()", `{ green: "grade-green", yellow: "grade-yellow", red: "grade-red" }`,
		"hasOwnProperty.call(GRADE_CLASS, d.grade)", `el("span", "cal-run", "•")`, "aria-label", "state.month >= todayString().slice(0, 7)")
	// 前端不寫門檻：沒有 300，也不對淨值做分級比較（門檻只在 Go 常數）.
	for _, bad := range []string{"300", "d.intake - ", "net <=", "net >="} {
		cal := jsFuncBody(t, js, "calendarCell") + jsFuncBody(t, js, "renderMonth")
		if strings.Contains(cal, bad) {
			t.Errorf("calendar code must not compute grade (found %q)", bad)
		}
	}
	// [F34-57] 詞界比對：編輯表單的範圍文字含「熱量 0～3000」，"3000" 含 "300"；只擋獨立的 300（淨值門檻）.
	if netBand.MatchString(js) {
		t.Error("app.js must not contain the net band threshold")
	}
}

// TestFrontendDayNavigationHasCatch [F30-24] 單日換日：失敗有 catch，只有 /day 成功才更新 state.date.
func TestFrontendDayNavigationHasCatch(t *testing.T) {
	js, html := frontendSources(t)
	mustContainAll(t, "index.html", html, `id="dv-back"`, "返回月曆", `id="dv-prev"`, `id="dv-next"`)
	load := jsFuncBody(t, js, "loadDay")
	if strings.Count(js, "state.date = date;") != 1 || !strings.Contains(load, "state.date = date;") {
		t.Errorf("state.date must only be assigned inside loadDay after a successful /day: %s", load)
	}
	dayIdx := strings.Index(load, `api("/day?date="`)
	if dayIdx < 0 {
		t.Fatalf(`loadDay has no api("/day?date=" request: %s`, load)
	}
	if !strings.Contains(load[dayIdx:], "state.date = date") {
		t.Error("state.date must be set after the /day request resolves")
	}
	if !strings.Contains(jsFuncBody(t, js, "goDay"), "runSection(") {
		t.Error("goDay must go through runSection so a failed switch shows an error")
	}
	step := jsFuncBody(t, js, "stepDay")
	mustContainAll(t, "stepDay", step, "todayString()", "goDay(shiftDate(state.date, delta))")
	if strings.Contains(js, "state.date = shiftDate") {
		t.Error("changing the day must not mutate state.date before the data is loaded")
	}
	mustContainAll(t, "app.js", js, "setMonth(state.date.slice(0, 7))") // 返回月曆回到該日所在月份
}

// TestFrontendForegroundReloadAndIndependentSections [F30-25] 回前景重載（兩個事件都掛、節流、未同意不重載）與各區獨立失敗.
func TestFrontendForegroundReloadAndIndependentSections(t *testing.T) {
	js, _ := frontendSources(t)
	mustContainAll(t, "app.js", js, `addEventListener("visibilitychange"`, `addEventListener("pageshow"`, "e.persisted", "now - lastReload < 5000", "reloading")
	reload := jsFuncBody(t, js, "reloadOnForeground")
	if !strings.Contains(reload, "!state.consented") {
		t.Errorf("foreground reload must be skipped before consent: %s", reload)
	}
	// 各區獨立：loadAll 不得再把各區綁成同一個會整頁失敗的 Promise.all(api(...)).
	if strings.Contains(js, "Promise.all([api(") {
		t.Error("sections must load independently, not in one Promise.all of api calls")
	}
	all := jsFuncBody(t, js, "runAllSections")
	if !strings.Contains(all, "runNamed") {
		t.Errorf("runAllSections must run each section through runNamed: %s", all)
	}
	mustContainAll(t, "app.js", js, `errors: ["today-error", "trend-error"]`, `errors: ["cal-error"]`, `errors: ["alerts-error"]`, `errors: ["account-error"]`, `errors: ["records-error", "dv-error"]`)
}

// cssBlock 取出 `selector {` 到下一個 `}` 之間的文字（app.css 沒有巢狀規則）.
func cssBlock(t *testing.T, css, selector string) string {
	t.Helper()
	_, rest, ok := strings.Cut(css, "\n"+selector+" {")
	if !ok {
		t.Fatalf("selector %q not found in app.css", selector)
	}
	body, _, ok := strings.Cut(rest, "}")
	if !ok {
		t.Fatalf("selector %q block is not closed", selector)
	}
	return body
}

// cssVarNames 取出區塊內宣告的 `--name:` 變數名（排序後）.
func cssVarNames(block string) []string {
	var names []string
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+):`).FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return names
}

// TestFrontendThemeToggle [F30-26] 預設深色、只有 data-theme="light" 才淺色、開關按過記在手機、不跟隨系統.
func TestFrontendThemeToggle(t *testing.T) {
	js, html := frontendSources(t)
	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)

	// ① 兩組變數：:root 是深色、:root[data-theme="light"] 是淺色，變數名集合相同.
	dark := cssBlock(t, css, ":root")
	light := cssBlock(t, css, `:root[data-theme="light"]`)
	mustContainAll(t, ":root", dark, "--ground: #12161d;", "color-scheme: dark;")
	mustContainAll(t, `:root[data-theme="light"]`, light, "--ground: #f6f7f9;", "color-scheme: light;")
	darkNames, lightNames := cssVarNames(dark), cssVarNames(light)
	if len(darkNames) < 18 || !slices.Equal(darkNames, lightNames) {
		t.Errorf("dark and light variable sets must match (>=18): dark=%v light=%v", darkNames, lightNames)
	}
	if strings.Contains(css, "prefers-color-scheme") {
		t.Error("app.css must not follow the system color scheme")
	}

	// ② 主題在 early return 之前套用（關卡 / 錯誤畫面也用選定主題）.
	earlyReturn := strings.Index(js, `typeof liff === "undefined"`)
	setAt := strings.Index(js, `dataset.theme = "light"`)
	callAt := strings.Index(js, "\n  initTheme();")
	if earlyReturn < 0 || setAt < 0 || setAt > earlyReturn {
		t.Errorf("theme must be set before the liff early return: set=%d early=%d", setAt, earlyReturn)
	}
	if callAt < 0 || callAt > earlyReturn {
		t.Errorf("initTheme() must be called before the liff early return: call=%d early=%d", callAt, earlyReturn)
	}

	// ③ 讀、寫同一把 key；click handler 會寫入，且寫入值與讀取比較的 "light" 一致.
	read := jsFuncBody(t, js, "readTheme")
	mustContainAll(t, "readTheme", read, "localStorage.getItem(THEME_KEY)", `=== "light"`)
	save := jsFuncBody(t, js, "saveTheme")
	mustContainAll(t, "saveTheme", save, "localStorage.setItem(THEME_KEY, theme)", "try {", "catch")
	init := jsFuncBody(t, js, "initTheme")
	mustContainAll(t, "initTheme", init, `addEventListener("click"`, "saveTheme(next)", "applyTheme(next)", `? "light" : "dark"`)
	if strings.Index(init, "applyTheme(next)") > strings.Index(init, "saveTheme(next)") {
		t.Error("the screen must switch before persisting, so a failed write still switches")
	}

	// ④ aria-checked = 目前是否深色.
	apply := jsFuncBody(t, js, "applyTheme")
	mustContainAll(t, "applyTheme", apply, `setAttribute("aria-checked", String(theme !== "light"))`)
	mustContainAll(t, "index.html", html, `id="theme-toggle"`, `role="switch"`, `class="theme-toggle"`)

	// ⑤ 不讀系統深淺色.
	if strings.Contains(js, "matchMedia") {
		t.Error("app.js must not read the system color scheme (matchMedia)")
	}
}

// TestFrontendEditRegionIsLinkStyle [F30-27] 通知列的「修改地區」是藍色連結，不被 `.item button`（紀錄刪除鈕）蓋成紅字加框.
func TestFrontendEditRegionIsLinkStyle(t *testing.T) {
	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	block := cssBlock(t, string(cssBytes), ".item .link")
	mustContainAll(t, ".item .link", block, "color: var(--accent)", "border: 0", "background: none", "font-size: 14px")
}

// TestFrontendNoDangerousSinks [F30-20..25] 新增的前端不使用 eval / document.write / console 輸出.
func TestFrontendNoDangerousSinks(t *testing.T) {
	js, _ := frontendSources(t)
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "eval(", "document.write", "console."} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js must not contain %q", bad)
		}
	}
	// localStorage 只准碰同意值與主題兩把 key（次數會隨功能增減，所以改檢查每次使用的第一個引數）.
	uses := regexp.MustCompile(`localStorage\.(?:getItem|setItem|removeItem)\(\s*([^,)\s]+)`).FindAllStringSubmatch(js, -1)
	if len(uses) == 0 {
		t.Error("expected localStorage uses for the consent and theme keys")
	}
	if all := strings.Count(js, "localStorage."); all != len(uses) {
		t.Errorf("every localStorage use must be getItem/setItem/removeItem with a key argument: %d uses, %d matched", all, len(uses))
	}
	for _, u := range uses {
		if u[1] != "CONSENT_KEY" && u[1] != "THEME_KEY" {
			t.Errorf("localStorage key %q is not allowed (only CONSENT_KEY, THEME_KEY)", u[1])
		}
	}
	for _, def := range []string{`var CONSENT_KEY = "consentVersion";`, `var THEME_KEY = "theme";`} {
		if n := strings.Count(js, def); n != 1 {
			t.Errorf("%q must be defined exactly once, got %d", def, n)
		}
	}
	if strings.Contains(js, "document.cookie") || strings.Contains(js, "sessionStorage") {
		t.Error("no cookies / sessionStorage")
	}
}

// TestFrontendNoThirdPartyFontsBeforeConsent [F302-08] 同意前頁面不對第三方字型服務送請求：HTML、CSS、CSP 都不出現字型主機.
func TestFrontendNoThirdPartyFontsBeforeConsent(t *testing.T) {
	_, html := frontendSources(t)
	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	hosts := []string{"fonts.googleapis.com", "fonts.gstatic.com"}
	for _, h := range hosts {
		if strings.Contains(html, h) {
			t.Errorf("index.html still references %s", h)
		}
		if strings.Contains(css, h) {
			t.Errorf("app.css still references %s", h)
		}
		if strings.Contains(contentSecurityPolicy, h) {
			t.Errorf("CSP still allows %s: %q", h, contentSecurityPolicy)
		}
	}
	for _, tag := range regexp.MustCompile(`(?s)<link\b[^>]*>`).FindAllString(html, -1) {
		if strings.Contains(tag, "https://") || strings.Contains(tag, "http://") || strings.Contains(tag, `href="//`) {
			t.Errorf("index.html <link> points off-site: %s", tag)
		}
	}
	if !strings.Contains(contentSecurityPolicy, "style-src 'self';") {
		t.Errorf("CSP style-src must be 'self' only: %q", contentSecurityPolicy)
	}
}

// TestFrontendChartColorsUseVariables [F302-04] 圖表與營養素條的顏色走 CSS 變數，深淺主題各一組，app.js 不寫死色碼.
func TestFrontendChartColorsUseVariables(t *testing.T) {
	js, _ := frontendSources(t)
	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)

	// (a) app.js 沒有 6 位色碼.
	if hits := regexp.MustCompile(`#[0-9a-fA-F]{6}`).FindAllString(js, -1); len(hits) != 0 {
		t.Errorf("app.js must not hardcode colors, found %v", hits)
	}

	// (b) 兩個主題區塊各自宣告三個營養素變數與指定的值.
	dark := cssBlock(t, css, ":root")
	light := cssBlock(t, css, `:root[data-theme="light"]`)
	mustContainAll(t, ":root", dark, "--macro-protein: #6aa8ff;", "--macro-carbs: #f0983a;", "--macro-fat: #ff7aa2;")
	mustContainAll(t, `:root[data-theme="light"]`, light, "--macro-protein: #1d6fd8;", "--macro-carbs: #d9790b;", "--macro-fat: #c2335f;")

	// (c) app.js 用到六個 class，app.css 對每個 class 有規則且規則內容走變數.
	rules := []struct{ class, decl string }{
		{"macro-protein", "var(--macro-protein)"},
		{"macro-carbs", "var(--macro-carbs)"},
		{"macro-fat", "var(--macro-fat)"},
		{"chart-intake", "fill: var(--intake)"},
		{"chart-burned", "fill: var(--burned)"},
		{"chart-base", "stroke: var(--base)"},
	}
	for _, r := range rules {
		if !strings.Contains(js, r.class) {
			t.Errorf("app.js does not use class %q", r.class)
		}
		rule := regexp.MustCompile(`(?s)\.` + regexp.QuoteMeta(r.class) + `\b[^{]*\{([^}]*)\}`).FindStringSubmatch(css)
		if rule == nil {
			t.Errorf("app.css has no rule for .%s", r.class)
			continue
		}
		if !strings.Contains(rule[1], r.decl) {
			t.Errorf("app.css .%s rule must contain %q, got %q", r.class, r.decl, rule[1])
		}
	}
}

// TestFrontendTabsUseAriaCurrent [F302-06] 分頁按鈕以 aria-current="page" 標示目前頁，不再用 aria-pressed；近 7 天 / 30 天的切換鈕仍用 aria-pressed.
func TestFrontendTabsUseAriaCurrent(t *testing.T) {
	js, html := frontendSources(t)
	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)

	// (a) index.html 五顆分頁鈕：只有 today 帶 aria-current="page"，都不含 aria-pressed.
	tabs := regexp.MustCompile(`<button[^>]*class="tab"[^>]*>`).FindAllString(html, -1)
	if len(tabs) != 5 {
		t.Fatalf("expected 5 tab buttons, got %d: %v", len(tabs), tabs)
	}
	for _, tab := range tabs {
		if strings.Contains(tab, "aria-pressed") {
			t.Errorf("tab button must not use aria-pressed: %s", tab)
		}
		isToday := strings.Contains(tab, `data-tab="today"`)
		if has := strings.Contains(tab, `aria-current="page"`); has != isToday {
			t.Errorf("only the today tab may start with aria-current=page (today=%v has=%v): %s", isToday, has, tab)
		}
	}

	// (b) selectTab 設定與移除 aria-current，不再碰 aria-pressed.
	sel := jsFuncBody(t, js, "selectTab")
	mustContainAll(t, "selectTab", sel, `setAttribute("aria-current", "page")`, `removeAttribute("aria-current")`)
	if strings.Contains(sel, "aria-pressed") {
		t.Error("selectTab must not touch aria-pressed")
	}

	// (c) CSS 選擇器跟著換.
	mustContainAll(t, "app.css", css, `.tab[aria-current="page"]`)
	if strings.Contains(css, ".tab[aria-pressed") {
		t.Error("app.css still styles .tab[aria-pressed]")
	}

	// (d) .seg 的 aria-pressed 不動.
	if n := strings.Count(html, `class="seg"`); n != 2 || strings.Count(html, "aria-pressed") != 2 {
		t.Errorf("index.html must keep exactly the two .seg aria-pressed (seg=%d pressed=%d)", n, strings.Count(html, "aria-pressed"))
	}
	mustContainAll(t, "app.js", js, `querySelectorAll(".seg").forEach(function (x) { x.setAttribute("aria-pressed"`)
	mustContainAll(t, "app.css", css, `.seg[aria-pressed="true"]`)
}

// TestFrontendCalendarGradeHasSecondChannel [F302-07] 月曆等級有第二通道：角落符號（aria-hidden）加 aria-label 等級文字，圖例同步.
func TestFrontendCalendarGradeHasSecondChannel(t *testing.T) {
	js, html := frontendSources(t)
	body := jsFuncBody(t, js, "calendarCell")

	markTable := `var GRADE_MARK = { green: "−", yellow: "≈", red: "+" };`
	wordTable := `var GRADE_WORD = { green: "熱量赤字", yellow: "接近平衡", red: "盈餘偏多" };`
	guard := func(x string) string {
		return "if (gradeOK && Object.prototype.hasOwnProperty.call(GRADE_" + x + ", d.grade))"
	}
	problems := func(src string) []string {
		var out []string
		wants := []string{"cal-grade", "aria-hidden", markTable, wordTable, "var gradeOK = hasBase && meals;", guard("CLASS"), guard("MARK"), guard("WORD")}
		for _, w := range wants {
			if !strings.Contains(src, w) {
				out = append(out, "calendarCell is missing "+w)
			}
		}
		return out
	}
	for _, p := range problems(body) {
		t.Error(p)
	}

	// 圖例：只看 #cal-legend 那個 <p>，不對整份 index.html 斷言.
	_, rest, ok := strings.Cut(html, `id="cal-legend"`)
	if !ok {
		t.Fatal("#cal-legend not found in index.html")
	}
	legend, _, ok := strings.Cut(rest, "</p>")
	if !ok {
		t.Fatal("#cal-legend paragraph is not closed")
	}
	mustContainAll(t, "#cal-legend", legend, "角落符號", "−", "≈", "+", "熱量赤字", "接近平衡", "盈餘偏多")

	// 變異：每一種都必須讓斷言變紅，否則這支測試守不住它宣稱要守的東西.
	mutations := []struct{ name, from, to string }{
		{"swap green and red marks", markTable, `var GRADE_MARK = { green: "+", yellow: "≈", red: "−" };`},
		{"mark append without gradeOK", guard("MARK"), "if (Object.prototype.hasOwnProperty.call(GRADE_MARK, d.grade))"},
		{"word append without gradeOK", guard("WORD"), "if (Object.prototype.hasOwnProperty.call(GRADE_WORD, d.grade))"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(body, m.from) != 1 {
				t.Fatalf("mutation anchor %q must appear exactly once in calendarCell", m.from)
			}
			if len(problems(strings.Replace(body, m.from, m.to, 1))) == 0 {
				t.Errorf("mutation %q went undetected", m.name)
			}
		})
	}
}

// TestFrontendCalendarLegendHasSwatches [F34-51]圖例是三個色塊加文字：HTML 有三個 aria-hidden 的 swatch，CSS 讓它有尺寸並引用等級底色變數.
func TestFrontendCalendarLegendHasSwatches(t *testing.T) {
	_, html := frontendSources(t)
	_, rest, ok := strings.Cut(html, `id="cal-legend"`)
	if !ok {
		t.Fatal("#cal-legend not found in index.html")
	}
	legend, _, ok := strings.Cut(rest, "</p>")
	if !ok {
		t.Fatal("#cal-legend paragraph is not closed")
	}
	for _, g := range []string{"green", "yellow", "red"} {
		mustContainAll(t, "#cal-legend", legend, `<span class="cal-swatch grade-`+g+`" aria-hidden="true"></span>`)
	}
	mustContainAll(t, "#cal-legend", legend, "•", "有運動")

	cssBytes, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	base := cssBlock(t, css, ".cal-swatch")
	if !strings.Contains(base, "display: inline-block") {
		t.Errorf(".cal-swatch must be display: inline-block, got %q", base)
	}
	for _, prop := range []string{"width", "height"} {
		m := regexp.MustCompile(`(?:^|[\s;])` + prop + `:\s*([0-9.]+)(px|em|rem)\b`).FindStringSubmatch(base)
		if m == nil {
			t.Errorf(".cal-swatch must declare a %s with a length unit, got %q", prop, base)
			continue
		}
		if v, err := strconv.ParseFloat(m[1], 64); err != nil || v <= 0 {
			t.Errorf(".cal-swatch %s must be non-zero, got %q", prop, m[1])
		}
	}
	for _, g := range []string{"green", "yellow", "red"} {
		rule := cssBlock(t, css, ".cal-swatch.grade-"+g)
		if !strings.Contains(rule, "background: var(--grade-"+g+"-bg)") {
			t.Errorf(".cal-swatch.grade-%s must use var(--grade-%s-bg), got %q", g, g, rule)
		}
	}
}

// TestFrontendDialogsTrapFocusAndRestore [F302-03] 刪除確認與地區選擇面板：焦點鎖在面板內、Escape 等同取消、關閉後焦點回開啟鈕、背景 inert.
func TestFrontendDialogsTrapFocusAndRestore(t *testing.T) {
	js, _ := frontendSources(t)
	// 只看非註解行，避免註解裡的字面讓斷言誤綠.
	var code []string
	for _, line := range strings.Split(js, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	src := strings.Join(code, "\n")

	mustContainAll(t, "app.js", src, "function openDialog(", "function closeDialog(", "openDialog(deleteBox", "openDialog(pickerBox",
		".inert = true", ".inert = false", `document.addEventListener("keydown"`, `"Escape"`)
	for _, bad := range []string{"deleteBox.hidden = true", "pickerBox.hidden = true"} {
		if strings.Contains(src, bad) {
			t.Errorf("app.js must close dialogs through closeDialog, found %q", bad)
		}
	}
	if !strings.Contains(jsFuncBody(t, js, "closePicker"), "closeDialog(") {
		t.Error("closePicker must call closeDialog(")
	}
	if !strings.Contains(jsFuncBody(t, js, "ensurePickerFocus"), "openBox === pickerBox") {
		t.Error("ensurePickerFocus must check openBox === pickerBox")
	}
	render := jsFuncBody(t, js, "renderPicker")
	if got, want := strings.Count(render, "ensurePickerFocus();"), strings.Count(render, "return;")+1; got < want {
		t.Errorf("renderPicker must call ensurePickerFocus() at every exit: %d calls, need >= %d (early returns + 1)", got, want)
	}

	// [F303-05] Tab / Shift+Tab 在面板內首尾循環：keydown 處理函式的字面與 focusablesIn 的過濾條件.
	_, kd, ok := strings.Cut(src, `document.addEventListener("keydown"`)
	if !ok {
		t.Fatal(`document.addEventListener("keydown" not found`)
	}
	kd, _, _ = strings.Cut(kd, "\n  });")
	for _, want := range []string{
		`e.key !== "Tab"`, "focusablesIn(openBox)", "items.length === 0", "at < 0",
		"e.shiftKey && at === 0", "!e.shiftKey && at === items.length - 1",
		"(e.shiftKey ? items[items.length - 1] : items[0]).focus()",
	} {
		if !strings.Contains(kd, want) {
			t.Errorf("keydown handler is missing the Tab-cycle literal %q", want)
		}
	}
	if n := strings.Count(kd, "e.preventDefault()"); n < 3 {
		t.Errorf("keydown handler has %d e.preventDefault() calls, need >= 3 (Escape, empty dialog, cycle)", n)
	}
	mustContainAll(t, "focusablesIn", jsFuncBody(t, js, "focusablesIn"), "!n.disabled", "!n.hidden", `getAttribute("tabindex") !== "-1"`)
}

// TestFrontendStaleLoadsDroppedAfterAccountDelete [F302-05] 刪除帳號後，刪除前送出的載入與變更請求回來一律丟棄：不寫畫面與 state、不觸發重載.
func TestFrontendStaleLoadsDroppedAfterAccountDelete(t *testing.T) {
	js, _ := frontendSources(t)
	const stale = "gen !== state.gen"

	// (a)(b) 世代欄位只在 afterAccountDeleted 遞增.
	mustContainAll(t, "app.js", js, "    gen: 0,")
	if n := strings.Count(js, "state.gen++"); n != 1 || !strings.Contains(jsFuncBody(t, js, "afterAccountDeleted"), "state.gen++") {
		t.Errorf("state.gen++ must appear exactly once, inside afterAccountDeleted (got %d)", n)
	}

	// (c) 五個載入函式、reloadTrend 與 runSection 都比對世代.
	for _, fn := range []string{"loadSummary", "loadDay", "loadMonth", "loadAlerts", "loadAccount", "reloadTrend", "runSection"} {
		body := jsFuncBody(t, js, fn)
		if !strings.Contains(body, "var gen = state.gen") || !strings.Contains(body, stale) {
			t.Errorf("%s must record var gen = state.gen and compare %q", fn, stale)
		}
	}

	// (d) 比對要在寫 state 之前.
	acct := jsFuncBody(t, js, "loadAccount")
	if g, w := strings.Index(acct, stale), strings.Index(acct, "state.synced = true"); g < 0 || w < 0 || g > w {
		t.Errorf("loadAccount must compare the generation before state.synced = true: guard=%d write=%d", g, w)
	}
	sum := jsFuncBody(t, js, "loadSummary")
	if g, w := strings.Index(sum, stale), strings.Index(sum, "state.summary = s"); g < 0 || w < 0 || g > w {
		t.Errorf("loadSummary must compare the generation before state.summary = s: guard=%d write=%d", g, w)
	}
	run := jsFuncBody(t, js, "runSection")
	if n := strings.Count(run, "if (gen !== state.gen) return true;"); n != 2 {
		t.Errorf("runSection must drop both the success and the failure branch when stale, got %d guards", n)
	}

	// (e) 三個變更型 handler 在發出當下記 gen，且每個 loadAll() 都被守住.
	for _, fn := range []string{"deleteButton", "scaleSelect", "saveProfile"} {
		body := jsFuncBody(t, js, fn)
		if !strings.Contains(body, "var gen = state.gen") {
			t.Errorf("%s must record var gen = state.gen when it sends", fn)
		}
		for from := 0; ; {
			i := strings.Index(body[from:], "loadAll()")
			if i < 0 {
				break
			}
			i += from
			from = i + len("loadAll()")
			before := body[:i]
			linePrefix := before[strings.LastIndex(before, "\n")+1:]
			callback := before[strings.LastIndex(before, "function ("):]
			if !strings.Contains(linePrefix, "gen === state.gen") && !strings.Contains(callback, stale) {
				t.Errorf("%s: loadAll() at offset %d is not guarded by the generation", fn, i)
			}
		}
	}
	mustContainAll(t, "app.js", js, `addEventListener("submit", saveProfile)`)

	// 直接重載的兩個 410 分支：quakeSwitch 與選擇器送出.
	const reload = "runAllSections().catch("
	if n := strings.Count(js, reload); n != 2 {
		t.Fatalf("expected 2 direct runAllSections().catch( calls, got %d", n)
	}
	for from := 0; ; {
		i := strings.Index(js[from:], reload)
		if i < 0 {
			break
		}
		i += from
		from = i + len(reload)
		// [F31-31] 守門錨在真正的提前 return：從最近一個 .catch(function 起算到重載呼叫之間必須有它（不是錨在恆真的 `gen === state.gen` 殘留條件）.
		catchStart := strings.LastIndex(js[:i], ".catch(function")
		if catchStart < 0 || !strings.Contains(js[catchStart:i], "if (gen !== state.gen) return;") {
			t.Errorf("[F31-31] runAllSections().catch( at offset %d must follow `if (gen !== state.gen) return;` inside its enclosing .catch(function body", i)
		}
		start := max(strings.LastIndex(js[:i], "\n  function "), strings.LastIndex(js[:i], ".addEventListener("))
		if !strings.Contains(js[start:i], "var gen = state.gen") {
			t.Errorf("the handler around offset %d must record var gen = state.gen before sending", i)
		}
	}
}

// TestFrontendAlertWritesDroppedAfterAccountDelete [F303-01] 刪帳號前送出的通知寫入，回應晚到時不得改 state、不得動新面板，開關照常解鎖.
func TestFrontendAlertWritesDroppedAfterAccountDelete(t *testing.T) {
	js, _ := frontendSources(t)
	const guard = "if (gen !== state.gen) return;"
	// section 從錨點切到下一個 `\n  });`（處理函式結尾）.
	section := func(anchor string) string {
		_, rest, ok := strings.Cut(js, anchor)
		if !ok {
			t.Fatalf("anchor %q not found in app.js", anchor)
		}
		body, _, _ := strings.Cut(rest, "\n  });")
		return body
	}
	// successBranch 取 `.then(function` 之後到 `.catch(` 之前（成功分支）.
	successBranch := func(name, src string) string {
		_, rest, ok := strings.Cut(src, ".then(function")
		if !ok {
			t.Errorf("%s has no .then(function success branch", name)
			return ""
		}
		body, _, _ := strings.Cut(rest, ".catch(")
		return body
	}
	// ordered 斷言 first 出現，且 laters 每一個若存在都在 first 之後.
	ordered := func(name, src, first string, laters ...string) {
		t.Helper()
		fi := strings.Index(src, first)
		if fi < 0 {
			t.Errorf("%s is missing %q", name, first)
			return
		}
		for _, l := range laters {
			if li := strings.Index(src, l); li >= 0 && li < fi {
				t.Errorf("%s: %q must come before %q", name, first, l)
			}
		}
	}

	quake := section(`quakeSwitch.addEventListener("click"`)
	weather := section(`weatherSwitch.addEventListener("click"`)
	picker := jsFuncBody(t, js, "submitPicker")

	for _, c := range []struct{ name, src string }{{"quakeSwitch click", quake}, {"weatherSwitch click", weather}, {"submitPicker", picker}} {
		mustContainAll(t, c.name+" (a)", c.src, "var gen = state.gen")
		if !strings.Contains(successBranch(c.name, c.src), guard) {
			t.Errorf("%s (b) success branch is missing %q", c.name, guard)
		}
	}
	ordered("weatherSwitch click (a)", weather, "var gen = state.gen", `api("/alerts/weather_warning", { method: "DELETE" })`)

	qOK, wOK, pOK := successBranch("quakeSwitch click", quake), successBranch("weatherSwitch click", weather), successBranch("submitPicker", picker)
	// (c) 開關解鎖在世代比對之前，否則舊回應 return 時開關永久 disabled.
	ordered("quakeSwitch success (c)", qOK, "quakeSwitch.disabled = false;", guard)
	ordered("weatherSwitch success (c)", wOK, "weatherSwitch.disabled = false;", guard)
	// (e) 世代比對在改 state / 重畫 / 提示之前.
	ordered("quakeSwitch success (e)", qOK, guard, "state.quake = !turningOff", "renderAlerts()", "setAlertsMessage(")
	ordered("weatherSwitch success (e)", wOK, guard, "state.weather = null", "renderAlerts()", "setAlertsMessage(")
	// (d) submitPicker 成功分支：世代比對在 closePicker() 與 state.weather = 之前.
	ordered("submitPicker success (d)", pOK, guard, "closePicker()", "state.weather =")
	// (f) submitPicker .catch 第一個敘述是世代比對.
	_, catchBody, ok := strings.Cut(picker, ".catch(function (err) {")
	if !ok {
		t.Fatal("submitPicker has no .catch(function (err) { branch")
	}
	if !strings.HasPrefix(strings.TrimSpace(catchBody), guard) {
		t.Errorf("submitPicker .catch (f) first statement must be %q", guard)
	}
	ordered("submitPicker catch (f)", catchBody, guard, "picker.submitting = false", "closePicker()", "setAlertsMessage(")
}

// TestFrontendFailureBranchesCheckGeneration [F31-10][F31-13] 通知開關的失敗分支與 ensureRegions 都比對世代；開關先解鎖再比對.
func TestFrontendFailureBranchesCheckGeneration(t *testing.T) {
	js, _ := frontendSources(t)
	const guard = "if (gen !== state.gen) return;"
	// [F31-31] 提前 return 之後的 `gen === state.gen` 恆真，不得留在程式裡.
	if strings.Contains(js, "&& gen === state.gen") {
		t.Error("[F31-31] app.js must not keep the always-true `&& gen === state.gen` after an early generation return")
	}
	catchOf := func(name, src string) string {
		_, rest, ok := strings.Cut(src, ".catch(function")
		if !ok {
			t.Fatalf("%s has no .catch(function branch", name)
		}
		return rest
	}
	cut := func(anchor string) string {
		_, rest, ok := strings.Cut(js, anchor)
		if !ok {
			t.Fatalf("anchor %q not found in app.js", anchor)
		}
		body, _, _ := strings.Cut(rest, "\n  });")
		return body
	}
	for _, c := range []struct{ name, src, unlock string }{
		{"quakeSwitch", cut(`quakeSwitch.addEventListener("click"`), "quakeSwitch.disabled = false;"},
		{"weatherSwitch", cut(`weatherSwitch.addEventListener("click"`), "weatherSwitch.disabled = false;"},
	} {
		catchBody := catchOf(c.name, c.src)
		ui, gi, mi := strings.Index(catchBody, c.unlock), strings.Index(catchBody, guard), strings.Index(catchBody, "setAlertsMessage(")
		if ui < 0 || gi < 0 || mi < 0 {
			t.Errorf("[F31-10] %s .catch must unlock, check the generation and write the message (unlock=%d guard=%d msg=%d)", c.name, ui, gi, mi)
			continue
		}
		if ui > gi || gi > mi {
			t.Errorf("[F31-10] %s .catch order must be unlock, generation check, message (unlock=%d guard=%d msg=%d)", c.name, ui, gi, mi)
		}
	}
	// [F31-35] deleteButton 的失敗分支（最後一個 .catch(function）：先解鎖、再比對世代、最後才顯示「刪除失敗」.
	del := jsFuncBody(t, js, "deleteButton")
	if ci := strings.LastIndex(del, ".catch(function"); ci < 0 {
		t.Error("[F31-35] deleteButton has no failure .catch(function branch")
	} else {
		fail := del[ci:]
		ui, gi, si := strings.Index(fail, "b.disabled = false;"), strings.Index(fail, guard), strings.Index(fail, `showStatus("刪除失敗`)
		if ui < 0 || gi < 0 || si < 0 || ui >= gi || gi >= si {
			t.Errorf("[F31-35] deleteButton failure branch must unlock, check the generation, then show the failure (unlock=%d guard=%d status=%d)", ui, gi, si)
		}
	}
	regions := jsFuncBody(t, js, "ensureRegions")
	mustContainAll(t, "ensureRegions [F31-13]", regions, "var gen = state.gen")
	if strings.Count(regions, "gen !== state.gen") < 2 {
		t.Error("[F31-13] ensureRegions must compare the generation in both the response and the failure branch")
	}
	if strings.Index(regions, "gen !== state.gen") > strings.Index(regions, "state.regionsData = ") {
		t.Error("[F31-13] ensureRegions must compare the generation before writing state.regionsData")
	}
}

// TestFrontendDialogCloseFallbacks [F31-12][F31-14] 防禦性字面守門（兩個狀態無法由真實操作達成）：關面板的焦點退路只選可用元素；刪帳號時地區面板一併收起.
func TestFrontendDialogCloseFallbacks(t *testing.T) {
	js, html := frontendSources(t)
	closeFn := jsFuncBody(t, js, "closeDialog")
	mustContainAll(t, "closeDialog [F31-12]", closeFn, "isUsable", `document.getElementById("alert-quake-switch")`, `document.getElementById("delete-open")`, ".find(isUsable)")
	// [F31-30] 第一個錨點是鏈本身（不是 `var opener = openOpener;`）；[F31-32] 鏈尾是 main，index.html 的 main 要能被程式聚焦.
	order := []string{`var chain = [opener,`, `box === deleteBox ? document.getElementById("delete-open") : weatherSwitch`, `document.getElementById("alert-quake-switch")`, `document.getElementById("delete-open"),`, `document.querySelector("main")]`}
	if !strings.Contains(html, `<main tabindex="-1">`) {
		t.Error(`[F31-32] index.html <main> must carry tabindex="-1" so the fallback chain can focus it`)
	}
	last := -1
	for _, w := range order {
		i := strings.Index(closeFn, w)
		if i <= last {
			t.Errorf("[F31-12] closeDialog fallback chain must list %q after the previous candidate", w)
		}
		last = i
	}
	usable := jsFuncBody(t, js, "isUsable")
	mustContainAll(t, "isUsable [F31-12]", usable, ".disabled", `closest("[hidden]")`, `closest("[inert]")`)
	// [F31-14] 不是目前面板的 box 也要 hidden = true（刪除確認是 openBox 時，地區面板靠這一行收起）.
	head, _, _ := strings.Cut(closeFn, "box.hidden = true;\n    dialogBackground")
	if !strings.Contains(head, "if (openBox !== box) {\n      box.hidden = true;") {
		t.Error("[F31-14] closeDialog must hide a box that is not the current one instead of returning")
	}
	after := jsFuncBody(t, js, "afterAccountDeleted")
	mustContainAll(t, "afterAccountDeleted [F31-14]", after, "closeDialog(deleteBox, false);", "closePicker(false);")
}

// TestFrontendDeleteCancelLockedWhileDeleting [F302-10] 刪除請求在途時取消鈕 disabled，失敗後與重開面板時解鎖，避免「取消」關掉面板但刪除照樣完成.
func TestFrontendDeleteCancelLockedWhileDeleting(t *testing.T) {
	js, _ := frontendSources(t)
	section := func(anchor string) string {
		_, rest, ok := strings.Cut(js, anchor)
		if !ok {
			t.Fatalf("anchor %q not found in app.js", anchor)
		}
		body, _, _ := strings.Cut(rest, "\n  });")
		return body
	}
	del := section(`getElementById("delete-ok").addEventListener("click"`)
	open := section(`getElementById("delete-open").addEventListener("click"`)

	const lock, unlock = `delete-cancel").disabled = true`, `delete-cancel").disabled = false`
	const call = `api("/account", { method: "DELETE" })`
	li, ci := strings.Index(del, lock), strings.Index(del, call)
	switch {
	case ci < 0:
		t.Errorf("delete-ok click is missing %q", call)
	case li < 0:
		t.Errorf("(a) delete-ok click is missing %q before the DELETE call", lock)
	case li > ci:
		t.Errorf("(a) %q must come before %q", lock, call)
	}
	_, catchBody, ok := strings.Cut(del, ".catch(")
	if !ok {
		t.Fatal("delete-ok click has no .catch( branch")
	}
	if !strings.Contains(catchBody, unlock) {
		t.Errorf("(b) delete-ok .catch is missing %q", unlock)
	}
	if !strings.Contains(open, unlock) {
		t.Errorf("(c) delete-open click is missing %q", unlock)
	}
}

// wcagLuminance 與 wcagContrast 依 WCAG 2.x 相對亮度公式（sRGB 線性化門檻 0.04045）算對比；hex 為 6 位 `#rrggbb`.
func wcagLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not a 6-digit hex color: %q", hex)
	}
	var lin [3]float64
	for i := range lin {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("bad hex color %q: %v", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			lin[i] = c / 12.92
		} else {
			lin[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*lin[0] + 0.7152*lin[1] + 0.0722*lin[2]
}

func wcagContrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := wcagLuminance(t, a), wcagLuminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// expandHex 把 3 位或 6 位十六進位色碼展開成小寫 6 位；其他格式回傳 ok=false.
func expandHex(h string) (string, bool) {
	h = strings.ToLower(h)
	if !regexp.MustCompile(`^#([0-9a-f]{3}|[0-9a-f]{6})$`).MatchString(h) {
		return "", false
	}
	if len(h) == 4 {
		return "#" + strings.Repeat(string(h[1]), 2) + strings.Repeat(string(h[2]), 2) + strings.Repeat(string(h[3]), 2), true
	}
	return h, true
}

// TestFrontendSwitchOnContrast [F303-03] 開啟中的開關：白鈕對 --switch-on 與 --switch-on 對 --card 在深淺兩主題都 ≥ 3:1（WCAG 1.4.11）.
func TestFrontendSwitchOnContrast(t *testing.T) {
	t.Run("formula positive control", func(t *testing.T) {
		if c := wcagContrast(t, "#ffffff", "#6aa8ff"); c >= 3.0 {
			t.Errorf("#ffffff on #6aa8ff = %.2f, want < 3.0 (the known failing pair)", c)
		}
		if c := wcagContrast(t, "#ffffff", "#000000"); math.Abs(c-21) > 0.01 {
			t.Errorf("#ffffff on #000000 = %.4f, want 21", c)
		}
	})

	b, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	block := func(anchor string) string {
		_, rest, ok := strings.Cut(css, anchor)
		if !ok {
			t.Fatalf("anchor %q not found in app.css", anchor)
		}
		body, _, _ := strings.Cut(rest, "}")
		return body
	}
	colorVar := func(theme, blk, name string) (string, bool) {
		m := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*(#[0-9a-fA-F]+)\s*;`).FindStringSubmatch(blk)
		if m == nil {
			t.Errorf("%s: %s is not defined as a hex color in the theme block", theme, name)
			return "", false
		}
		h, ok := expandHex(m[1])
		if !ok {
			t.Errorf("%s: %s = %q is not a 3- or 6-digit hex color", theme, name, m[1])
		}
		return h, ok
	}

	knobM := regexp.MustCompile(`\.switch::after\s*\{[^}]*background:\s*(#[0-9a-fA-F]+)\s*;`).FindStringSubmatch(css)
	if knobM == nil {
		t.Fatal(".switch::after rule has no hex background (the knob color)")
	}
	knob, ok := expandHex(knobM[1])
	if !ok {
		t.Fatalf(".switch::after background %q is not a 3- or 6-digit hex color", knobM[1])
	}

	for _, th := range []struct{ name, anchor string }{
		{"dark", ":root {"},
		{"light", `:root[data-theme="light"] {`},
	} {
		blk := block(th.anchor)
		on, okOn := colorVar(th.name, blk, "--switch-on")
		card, okCard := colorVar(th.name, blk, "--card")
		if okOn {
			if c := wcagContrast(t, knob, on); c < 3.0 {
				t.Errorf("(a) %s: knob %s on --switch-on %s = %.2f, want >= 3.0", th.name, knob, on, c)
			}
		}
		if okOn && okCard {
			if c := wcagContrast(t, on, card); c < 3.0 {
				t.Errorf("(b) %s: --switch-on %s on --card %s = %.2f, want >= 3.0", th.name, on, card, c)
			}
		}
	}

	rule := regexp.MustCompile(`\.switch\[aria-checked="true"\]\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if rule == nil {
		t.Fatal(`(c) .switch[aria-checked="true"] rule not found`)
	}
	if !strings.Contains(rule[1], "var(--switch-on)") || strings.Contains(rule[1], "var(--accent)") {
		t.Errorf(`(c) .switch[aria-checked="true"] must use var(--switch-on) and not var(--accent), got %q`, strings.TrimSpace(rule[1]))
	}
}

// ---- [F34-53..56] LIFF 直接編輯餐點與運動 ----

// putMealFixture 一筆 Version 為 0 的舊紀錄（上線前就存在，沒有 version 欄位）與一個體重 70 的身體資料.
func putMealFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPI(t)
	m := meal.Meal{
		ID: "m1", UserID: "U1", EatenAt: fixedNow.Add(-2 * time.Hour), MealType: "lunch", Note: "午餐吃了牛肉麵",
		Items: []meal.FoodItem{{Name: "豚骨拉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}, Source: meal.ItemFromAI}},
		Scale: 0.5,
	}
	m.Recalc()
	f.meals.Items = []meal.Meal{m}
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}
	return f
}

func putDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// putDayRecord 從 GET /me/day 取出第一筆原始 JSON 物件（kind = "meals" 或 "workouts"）.
func putDayRecord(t *testing.T, f *apiFixture, kind string) map[string]any {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/v1/me/day?date=2026-11-02", "good-U1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/day: status %d: %s", rec.Code, rec.Body.String())
	}
	list, _ := putDecode(t, rec)[kind].([]any)
	if len(list) == 0 {
		t.Fatalf("GET /me/day: no %s: %s", kind, rec.Body.String())
	}
	obj, _ := list[0].(map[string]any)
	return obj
}

// putBody 組出 PUT body；version 與 items 以外的欄位用 extra 夾帶.
func putBody(t *testing.T, version, items any, extra map[string]any) string {
	t.Helper()
	m := map[string]any{}
	if version != nil {
		m["version"] = version
	}
	if items != nil {
		m["items"] = items
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func putItem(name, portion string, cal, protein, carbs, fat float64) map[string]any {
	return map[string]any{"name": name, "portion": portion, "calories": cal, "protein_g": protein, "carbs_g": carbs, "fat_g": fat}
}

// putStats 連打 day / summary / month，回傳 11-02 當天的攝取（day 明細合計, summary 今日, month 當天）與 month 當天 grade.
func putStats(t *testing.T, f *apiFixture) (dayIntake, summaryIntake, monthIntake float64, grade string) {
	t.Helper()
	var day DayRecords
	if rec := f.do(http.MethodGet, "/api/v1/me/day?date=2026-11-02", "good-U1", ""); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &day) != nil {
		t.Fatalf("day: %d %s", rec.Code, rec.Body.String())
	}
	for i := range day.Meals {
		dayIntake += day.Meals[i].Total.Calories
	}
	var sum SummaryView
	if rec := f.do(http.MethodGet, "/api/v1/me/summary?days=1", "good-U1", ""); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &sum) != nil {
		t.Fatalf("summary: %d %s", rec.Code, rec.Body.String())
	}
	month := acctDecodeMonth(t, f.do(http.MethodGet, acctMonthPath+"2026-11", "good-U1", "").Body.Bytes())
	return dayIntake, sum.Today.Intake, month.Days[1].Intake, month.Days[1].Grade
}

// TestPutMealItemsUpdatesStatsImmediately [F34-53] C-1：PUT 後同一測試內連打 day / summary / month 都是新值；
// 舊紀錄（Version 0）的 view 必有 version 鍵且值為 0；成功後 Rebase：view 沒有 scale、儲存的 Scale 為 0；夾帶欄位一律忽略.
func TestPutMealItemsUpdatesStatsImmediately(t *testing.T) {
	f := putMealFixture(t)

	rec := putDayRecord(t, f, "meals")
	v, ok := rec["version"]
	if !ok || v != float64(0) {
		t.Fatalf("a Version-0 record must still carry the version key: version=%v present=%v record=%v", v, ok, rec)
	}
	if rec["scale"] != 0.5 {
		t.Fatalf("precondition: the fixture meal must carry scale 0.5, got %v", rec["scale"])
	}

	body := putBody(t, v, []any{putItem("家常拉麵", "1碗", 480, 20, 70, 12)}, map[string]any{
		"userId": "other", "total": map[string]any{"calories": 1}, "eatenAt": "2000-01-01T00:00:00Z", "scale": 3, "id": "m9",
	})
	res := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", body)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT: status %d: %s", res.Code, res.Body.String())
	}
	got := putDecode(t, res)
	if got["version"] != float64(1) || got["id"] != "m1" {
		t.Errorf("response version/id = %v/%v, want 1/m1: %s", got["version"], got["id"], res.Body.String())
	}
	if _, has := got["scale"]; has {
		t.Errorf("scale must be absent after an edit (Rebase): %s", res.Body.String())
	}
	var view MealView
	_ = json.Unmarshal(res.Body.Bytes(), &view)
	if len(view.Items) != 1 || view.Items[0].Name != "家常拉麵" || view.Total.Calories != 480 || view.Total.ProteinG != 20 {
		t.Errorf("response = %s", res.Body.String())
	}
	stored := f.meals.Items[0]
	if !stored.EatenAt.Equal(fixedNow.Add(-2*time.Hour)) || stored.Total.Calories != 480 || stored.Scale != 0 || stored.UserID != "U1" || stored.ID != "m1" {
		t.Errorf("body-supplied id/userId/total/eatenAt/scale must be ignored: %+v", stored)
	}

	dayIntake, summaryIntake, monthIntake, grade := putStats(t, f)
	if dayIntake != 480 || summaryIntake != 480 || monthIntake != 480 || grade != gradeGreen {
		t.Errorf("stats after edit: day=%v summary=%v month=%v grade=%q, want 480/480/480/green", dayIntake, summaryIntake, monthIntake, grade)
	}

	// Negative：items 空陣列 → 400 且訊息含 items，資料不變.
	before := f.meals.Items[0]
	empty := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, got["version"], []any{}, nil))
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), meal.FieldItems) {
		t.Errorf("empty items: status %d: %s", empty.Code, empty.Body.String())
	}
	if !reflect.DeepEqual(before, f.meals.Items[0]) {
		t.Error("a rejected PUT must not change the meal")
	}
	if rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d", rec.Code)
	}
}

// TestPutOnlyChangesTheCallersRecords [F34-53][F34-56] C-7：他人的 id 是 404，對方紀錄不變；夾帶的 userId / total 被忽略；自己的同 id 正常.
func TestPutOnlyChangesTheCallersRecords(t *testing.T) {
	t.Run("meal", func(t *testing.T) {
		f := putMealFixture(t)
		theirs := f.meals.Items[0]
		theirs.ID, theirs.UserID = "theirs", "U2"
		f.meals.Items = append(f.meals.Items, theirs)
		body := putBody(t, 0, []any{putItem("改過", "", 1, 1, 1, 1)}, map[string]any{"userId": "U2", "total": map[string]any{"calories": 999}, "eatenAt": "2000-01-01T00:00:00Z", "scale": 2})

		if rec := f.do(http.MethodPut, "/api/v1/me/meals/theirs", "good-U1", body); rec.Code != http.StatusNotFound {
			t.Errorf("another user's meal: status %d", rec.Code)
		}
		if !reflect.DeepEqual(f.meals.Items[1], theirs) {
			t.Errorf("another user's meal must not change: %+v", f.meals.Items[1])
		}
		if rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", body); rec.Code != http.StatusOK {
			t.Fatalf("own meal: status %d: %s", rec.Code, rec.Body.String())
		}
		if got := f.meals.Items[0]; got.UserID != "U1" || got.Total.Calories != 1 || got.Scale != 0 || !got.EatenAt.Equal(fixedNow.Add(-2*time.Hour)) {
			t.Errorf("total comes from the items, not the body: %+v", got)
		}
	})
	t.Run("workout", func(t *testing.T) {
		f := putWorkoutFixture(t)
		theirs := f.workouts.Items[0]
		theirs.ID, theirs.UserID = "theirs", "U2"
		f.workouts.Items = append(f.workouts.Items, theirs)
		if rec := f.do(http.MethodPut, "/api/v1/me/workouts/theirs", "good-U1", putWorkoutBody(t, 0, map[string]any{"calories": 400})); rec.Code != http.StatusNotFound {
			t.Errorf("another user's workout: status %d", rec.Code)
		}
		if !reflect.DeepEqual(f.workouts.Items[1], theirs) {
			t.Errorf("another user's workout must not change: %+v", f.workouts.Items[1])
		}
		if rec := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, 0, map[string]any{"calories": 400, "userId": "U2"})); rec.Code != http.StatusOK {
			t.Errorf("own workout: status %d", rec.Code)
		}
	})
}

// TestPutMealValidationMatrix [F34-54] C-2：驗證不過一律 400（2001）且文件逐欄不變；邊界值（=上限）通過、（=上限+1）被拒.
// 邊界數字（3000 / 3001、30 / 31、500 / 501、20 / 21）取自規格表而不是 meal 套件的常數：改常數時這張表要跟著紅.
// 單品熱量上限的唯一定義是 meal.MaxItemCalories；常數本身另由 TestValidateItemsRangesArePinned 釘住.
func TestPutMealValidationMatrix(t *testing.T) {
	good := func() map[string]any { return putItem("家常拉麵", "1碗", 480, 20, 70, 12) }
	with := func(k string, v any) []any { it := good(); it[k] = v; return []any{it} }
	many := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			it := good()
			it["calories"] = 100
			out[i] = it
		}
		return out
	}
	rejects := []struct {
		name string
		body func(t *testing.T) string
		want string // 訊息裡要指出的欄位
	}{
		{"items empty", func(t *testing.T) string { return putBody(t, 0, []any{}, nil) }, meal.FieldItems},
		{"items missing", func(t *testing.T) string { return putBody(t, 0, nil, nil) }, meal.FieldItems},
		{"name empty", func(t *testing.T) string { return putBody(t, 0, with("name", ""), nil) }, meal.FieldName},
		{"name blank", func(t *testing.T) string { return putBody(t, 0, with("name", "   "), nil) }, meal.FieldName},
		{"name too long", func(t *testing.T) string {
			return putBody(t, 0, with("name", strings.Repeat("字", 31)), nil)
		}, meal.FieldName},
		{"name has URL", func(t *testing.T) string { return putBody(t, 0, with("name", "看 http://x.co"), nil) }, meal.FieldName},
		{"portion too long", func(t *testing.T) string {
			return putBody(t, 0, with("portion", strings.Repeat("字", 31)), nil)
		}, meal.FieldPortion},
		{"portion has URL", func(t *testing.T) string { return putBody(t, 0, with("portion", "http://x.co"), nil) }, meal.FieldPortion},
		{"calories negative", func(t *testing.T) string { return putBody(t, 0, with("calories", -1), nil) }, meal.FieldCalories},
		{"calories over", func(t *testing.T) string { return putBody(t, 0, with("calories", 3001), nil) }, meal.FieldCalories},
		{"protein negative", func(t *testing.T) string { return putBody(t, 0, with("protein_g", -1), nil) }, meal.FieldProtein},
		{"protein over", func(t *testing.T) string { return putBody(t, 0, with("protein_g", 501), nil) }, meal.FieldProtein},
		{"carbs negative", func(t *testing.T) string { return putBody(t, 0, with("carbs_g", -1), nil) }, meal.FieldCarbs},
		{"carbs over", func(t *testing.T) string { return putBody(t, 0, with("carbs_g", 501), nil) }, meal.FieldCarbs},
		{"fat negative", func(t *testing.T) string { return putBody(t, 0, with("fat_g", -1), nil) }, meal.FieldFat},
		{"fat over", func(t *testing.T) string { return putBody(t, 0, with("fat_g", 501), nil) }, meal.FieldFat},
		{"too many items", func(t *testing.T) string { return putBody(t, 0, many(21), nil) }, meal.FieldItems},
		{"null item", func(t *testing.T) string { return `{"version":0,"items":[null]}` }, meal.FieldName},
		{"not JSON", func(t *testing.T) string { return `not json` }, "invalid body"},
		{"version missing", func(t *testing.T) string { return putBody(t, nil, []any{good()}, nil) }, "version"},
		{"version not a number", func(t *testing.T) string { return putBody(t, "abc", []any{good()}, nil) }, "invalid body"},
		{"calories not a number", func(t *testing.T) string { return putBody(t, 0, with("calories", "lots"), nil) }, "invalid body"},
	}
	for _, c := range rejects {
		t.Run("reject/"+c.name, func(t *testing.T) {
			f := putMealFixture(t)
			before := f.meals.Items[0]
			rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", c.body(t))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"2001"`) || !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("status %d, want 400 / 2001 naming %q: %s", rec.Code, c.want, rec.Body.String())
			}
			if !reflect.DeepEqual(before, f.meals.Items[0]) {
				t.Errorf("a rejected PUT must not change the meal: %+v", f.meals.Items[0])
			}
		})
	}

	accepts := []struct {
		name  string
		items []any
	}{
		{"calories 0 and 3000", []any{func() any { it := good(); it["calories"] = 0; return it }(), func() any { it := good(); it["calories"] = 3000; return it }()}},
		{"name at max", with("name", strings.Repeat("字", 30))},
		{"portion empty", with("portion", "")},
		{"portion at max", with("portion", strings.Repeat("字", 30))},
		{"macros 0", []any{func() any { it := good(); it["protein_g"], it["carbs_g"], it["fat_g"] = 0, 0, 0; return it }()}},
		{"macros at max", []any{func() any {
			it := good()
			it["protein_g"], it["carbs_g"], it["fat_g"] = 500, 500, 500
			return it
		}()}},
		{"one item", many(1)},
		{"items at max", many(20)},
	}
	for _, c := range accepts {
		t.Run("accept/"+c.name, func(t *testing.T) {
			f := putMealFixture(t)
			rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, c.items, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(f.meals.Items[0].Items) != len(c.items) {
				t.Errorf("stored %d items, want %d", len(f.meals.Items[0].Items), len(c.items))
			}
		})
	}
}

// legacyMeal 舊資料：22 個品項，其中一個單品 6000 大卡（SetTotalCalories 能推到的值）、一個份量 25 字；EditStored 造出來，不經 PUT 驗證.
func legacyMeal(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPI(t)
	items := make([]meal.FoodItem, 22)
	for i := range items {
		items[i] = meal.FoodItem{Name: "品項" + strconv.Itoa(i), Portion: "1份", Nutrients: meal.Nutrients{Calories: 10}, Source: meal.ItemFromAI}
	}
	items[0] = meal.FoodItem{Name: "大餐", Portion: "1份", Nutrients: meal.Nutrients{Calories: 6000}, Source: meal.ItemFromAI}
	items[1].Portion = strings.Repeat("份", 25)
	f.meals.Items = []meal.Meal{{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: items}}
	f.meals.EditStored(0, func(m *meal.Meal) { m.Recalc() })
	return f
}

// legacyBody 把既有品項原樣送回，再套用 mutate.
func legacyBody(t *testing.T, f *apiFixture, mutate func(items []map[string]any) []map[string]any) string {
	t.Helper()
	var items []map[string]any
	for _, it := range f.meals.Items[0].Items {
		items = append(items, putItem(it.Name, it.Portion, it.Calories, it.ProteinG, it.CarbsG, it.FatG))
	}
	out := mutate(items)
	anyItems := make([]any, len(out))
	for i := range out {
		anyItems[i] = out[i]
	}
	return putBody(t, f.meals.Items[0].Version, anyItems, nil)
}

// TestPutMealLegacyItemsStayEditable [F34-54] C-3：舊資料的 6000 大卡單品、25 字份量、22 個品項在只改別的品項時不被範圍擋下.
func TestPutMealLegacyItemsStayEditable(t *testing.T) {
	t.Run("rename another item keeps the legacy ones untouched", func(t *testing.T) {
		f := legacyMeal(t)
		before := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", legacyBody(t, f, func(items []map[string]any) []map[string]any {
			items[5]["name"] = "改名後"
			return items
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		got := f.meals.Items[0].Items
		if len(got) != 22 || got[0].Calories != 6000 || got[0].Source != meal.ItemFromAI || got[1].Portion != before.Items[1].Portion || got[5].Name != "改名後" || got[5].Source != meal.ItemFromUser {
			t.Errorf("stored = %+v", got)
		}
	})
	t.Run("raising the legacy item to 6001 is rejected", func(t *testing.T) {
		f := legacyMeal(t)
		before := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", legacyBody(t, f, func(items []map[string]any) []map[string]any {
			items[0]["calories"] = 6001
			return items
		}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), meal.FieldCalories) {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(before, f.meals.Items[0]) {
			t.Error("a rejected PUT must not change the meal")
		}
	})
	t.Run("adding a 23rd item is rejected", func(t *testing.T) {
		f := legacyMeal(t)
		before := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", legacyBody(t, f, func(items []map[string]any) []map[string]any {
			return append(items, putItem("新增", "", 1, 0, 0, 0))
		}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), meal.FieldItems) {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(before, f.meals.Items[0]) {
			t.Error("a rejected PUT must not change the meal")
		}
	})
}

// TestPutMealTotalCap [F34-54] C-4：整餐熱量上限 6000；舊資料合計已超過時，只要不高於改前就放行.
func TestPutMealTotalCap(t *testing.T) {
	threeItems := func(a, b, c float64) []any {
		return []any{putItem("甲", "", a, 0, 0, 0), putItem("乙", "", b, 0, 0, 0), putItem("丙", "", c, 0, 0, 0)}
	}
	t.Run("100 kcal meal pushed to 6001", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.Items[0].Items[0].Calories = 100
		f.meals.Items[0].Recalc()
		before := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, threeItems(3000, 3000, 1), nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), meal.FieldCalories) {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(before, f.meals.Items[0]) {
			t.Error("a rejected PUT must not change the meal")
		}
	})
	t.Run("6000 meal stays 6000 when only a name changes", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.EditStored(0, func(m *meal.Meal) {
			m.Items = []meal.FoodItem{
				{Name: "甲", Nutrients: meal.Nutrients{Calories: 3000}}, {Name: "乙", Nutrients: meal.Nutrients{Calories: 3000}},
			}
			m.Recalc()
		})
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 1, []any{putItem("甲", "", 3000, 0, 0, 0), putItem("乙改", "", 3000, 0, 0, 0)}, nil))
		if rec.Code != http.StatusOK || f.meals.Items[0].Total.Calories != 6000 {
			t.Errorf("status %d total %v: %s", rec.Code, f.meals.Items[0].Total.Calories, rec.Body.String())
		}
		// 對照：同一餐改成 6001 → 400.
		g := putMealFixture(t)
		g.meals.EditStored(0, func(m *meal.Meal) {
			m.Items = []meal.FoodItem{{Name: "甲", Nutrients: meal.Nutrients{Calories: 3000}}, {Name: "乙", Nutrients: meal.Nutrients{Calories: 3000}}}
			m.Recalc()
		})
		rec = g.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 1, []any{putItem("甲", "", 3000, 0, 0, 0), putItem("乙", "", 3000, 0, 0, 0), putItem("丙", "", 1, 0, 0, 0)}, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), meal.FieldCalories) {
			t.Errorf("6000 -> 6001: status %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("legacy total above the cap may stay but not grow", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.EditStored(0, func(m *meal.Meal) {
			m.Items = []meal.FoodItem{
				{Name: "甲", Nutrients: meal.Nutrients{Calories: 3000}}, {Name: "乙", Nutrients: meal.Nutrients{Calories: 3500}}, {Name: "丙", Nutrients: meal.Nutrients{Calories: 5}},
			}
			m.Recalc()
		})
		// 乙 3500 超過單品上限但沒動；只改丙的名字：合計 6505 沒有增加.
		same := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 1, []any{
			putItem("甲", "", 3000, 0, 0, 0), putItem("乙", "", 3500, 0, 0, 0), putItem("丙改", "", 5, 0, 0, 0),
		}, nil))
		if same.Code != http.StatusOK {
			t.Fatalf("unchanged total: status %d: %s", same.Code, same.Body.String())
		}
		grow := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 2, []any{
			putItem("甲", "", 3000, 0, 0, 0), putItem("乙", "", 3500, 0, 0, 0), putItem("丙改", "", 6, 0, 0, 0),
		}, nil))
		if grow.Code != http.StatusBadRequest || !strings.Contains(grow.Body.String(), meal.FieldCalories) {
			t.Errorf("grown total: status %d: %s", grow.Code, grow.Body.String())
		}
	})
}

// TestPutStaleVersionConflicts [F34-53] C-6：聊天更正使版本前進後，舊 version 的 PUT 回 409（4009），文件保持聊天更正後的樣子；
// 讀出之後才被別處改走一樣 409；帶目前版本 200；version 缺省是 400 不是 409.
func TestPutStaleVersionConflicts(t *testing.T) {
	t.Run("(a) stale version after a chat edit", func(t *testing.T) {
		f := putMealFixture(t)
		staleVersion := putDayRecord(t, f, "meals")["version"] // 使用者打開表單時拿到的版本
		f.meals.EditStored(0, func(m *meal.Meal) {
			m.Items = []meal.FoodItem{{Name: "聊天更正後", Nutrients: meal.Nutrients{Calories: 400}}}
			m.Rebase()
		})
		after := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, staleVersion, []any{putItem("我的編輯", "", 100, 1, 1, 1)}, nil))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"4009"`) {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(after, f.meals.Items[0]) {
			t.Errorf("the chat edit must survive: %+v", f.meals.Items[0])
		}
	})
	t.Run("(b) changed elsewhere between read and write", func(t *testing.T) {
		g := putMealFixture(t)
		g.meals.BeforeUpdate = func(m *storagetest.Meals) {
			m.BeforeUpdate = nil
			m.Items[0].Items[0].Name = "別處改的"
			m.Items[0].Version++
		}
		race := g.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, []any{putItem("我的編輯", "", 100, 1, 1, 1)}, nil))
		if race.Code != http.StatusConflict || g.meals.Items[0].Items[0].Name != "別處改的" {
			t.Errorf("race: status %d, stored %+v", race.Code, g.meals.Items[0].Items)
		}
	})
	t.Run("(c) the current version succeeds", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.EditStored(0, func(m *meal.Meal) { m.Note = "改過" })
		cur := putDayRecord(t, f, "meals")["version"]
		if cur != float64(1) {
			t.Fatalf("precondition: version = %v, want 1", cur)
		}
		if ok := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, cur, []any{putItem("我的編輯", "", 100, 1, 1, 1)}, nil)); ok.Code != http.StatusOK {
			t.Errorf("current version: status %d: %s", ok.Code, ok.Body.String())
		}
	})
	t.Run("missing version is 400, not 409", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.EditStored(0, func(m *meal.Meal) { m.Note = "改過" })
		if rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, nil, []any{putItem("x", "", 1, 0, 0, 0)}, nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestPutMealKeepsSourceOfUntouchedItems [F34-55] C-5：以內容（名稱＋四個營養值）配對沿用 Source，改過的標 ItemFromUser；每列斷言 FindByID 讀回的值.
func TestPutMealKeepsSourceOfUntouchedItems(t *testing.T) {
	rice := func() map[string]any { return putItem("白飯", "1碗", 366, 6.2, 82, 0.6) }
	egg := func() map[string]any { return putItem("滷蛋", "1顆", 80, 7, 1, 5) }
	veg := func() map[string]any { return putItem("燙青菜", "1份", 30, 1, 4, 1) }
	cases := []struct {
		name  string
		items []any
		want  map[string]string // 品名 -> Source
	}{
		{"delete the first item, the others keep their source (content match, not index)", []any{egg(), veg()}, map[string]string{"滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"calories changed", []any{func() any { r := rice(); r["calories"] = 300; return r }(), egg(), veg()}, map[string]string{"白飯": meal.ItemFromUser, "滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"portion text only keeps the source", []any{rice(), egg(), func() any { v := veg(); v["portion"] = "半份"; return v }()}, map[string]string{"白飯": meal.ItemFromTable, "滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"name changed", []any{func() any { r := rice(); r["name"] = "糙米飯"; return r }(), egg(), veg()}, map[string]string{"糙米飯": meal.ItemFromUser, "滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"a macro changed", []any{func() any { r := rice(); r["fat_g"] = 0.7; return r }(), egg(), veg()}, map[string]string{"白飯": meal.ItemFromUser, "滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"nothing changed", []any{rice(), egg(), veg()}, map[string]string{"白飯": meal.ItemFromTable, "滷蛋": meal.ItemFromAI, "燙青菜": ""}},
		{"egg renamed, rice untouched, veg portion only", []any{rice(), func() any { e := egg(); e["name"] = "鹹蛋"; return e }(), func() any { v := veg(); v["portion"] = "半份"; return v }()}, map[string]string{"白飯": meal.ItemFromTable, "鹹蛋": meal.ItemFromUser, "燙青菜": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAPI(t)
			m := meal.Meal{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: []meal.FoodItem{
				{Name: "白飯", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 366, ProteinG: 6.2, CarbsG: 82, FatG: 0.6}, Source: meal.ItemFromTable},
				{Name: "滷蛋", Portion: "1顆", Nutrients: meal.Nutrients{Calories: 80, ProteinG: 7, CarbsG: 1, FatG: 5}, Source: meal.ItemFromAI},
				{Name: "燙青菜", Portion: "1份", Nutrients: meal.Nutrients{Calories: 30, ProteinG: 1, CarbsG: 4, FatG: 1}}, // 空 Source（舊資料）
			}}
			m.Recalc()
			f.meals.Items = []meal.Meal{m}
			if rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, c.items, nil)); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			got, err := f.meals.FindByID(context.Background(), "U1", "m1")
			if err != nil || got == nil {
				t.Fatalf("FindByID: %v %v", got, err)
			}
			if len(got.Items) != len(c.want) {
				t.Fatalf("stored %d items, want %d: %+v", len(got.Items), len(c.want), got.Items)
			}
			for _, it := range got.Items {
				if want, ok := c.want[it.Name]; !ok || it.Source != want {
					t.Errorf("%s: stored Source %q, want %q", it.Name, it.Source, want)
				}
			}
		})
	}
}

// TestPutMealMatchesEachExistingItemOnce [F34-55] C-5：同名同數值的兩個品項，每個既有品項只能被配對一次，多出來的那個是新組合（標使用者值）.
func TestPutMealMatchesEachExistingItemOnce(t *testing.T) {
	f := newAPI(t)
	m := meal.Meal{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: []meal.FoodItem{
		{Name: "茶葉蛋", Nutrients: meal.Nutrients{Calories: 70}, Source: meal.ItemFromTable},
	}}
	m.Recalc()
	f.meals.Items = []meal.Meal{m}
	egg := func() map[string]any { return putItem("茶葉蛋", "", 70, 0, 0, 0) }
	if rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, []any{egg(), egg()}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := f.meals.Items[0].Items
	if len(got) != 2 || got[0].Source != meal.ItemFromTable || got[1].Source != meal.ItemFromUser {
		t.Errorf("sources = %+v", got)
	}
}

// TestPutMealSanitizesEditedText [F34-54] 有改的品項名稱與份量先用聊天路徑同一個 SanitizeDisplay 清理再驗證；沒改的品項原樣保留；清理後名稱為空 → 400 點名 name.
func TestPutMealSanitizesEditedText(t *testing.T) {
	dirty := []string{"牛肉\n麵", "牛\t肉  麵", "牛肉\x07麵", "[[記錄]]牛肉麵", "  牛肉   麵  ", "牛肉麵]]"}
	for _, raw := range dirty {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			f := putMealFixture(t)
			rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, []any{putItem(raw, raw+"一碗", 480, 20, 70, 12)}, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			got, err := f.meals.FindByID(context.Background(), "U1", "m1")
			if err != nil || got == nil || len(got.Items) != 1 {
				t.Fatalf("FindByID: %v %v", got, err)
			}
			wantName, wantPortion := recordedit.SanitizeDisplay(raw, 0), recordedit.SanitizeDisplay(raw+"一碗", 0)
			if got.Items[0].Name != wantName || got.Items[0].Portion != wantPortion {
				t.Errorf("stored name %q portion %q, want %q / %q", got.Items[0].Name, got.Items[0].Portion, wantName, wantPortion)
			}
			if wantName == raw {
				t.Errorf("precondition: %q must be changed by the sanitizer", raw)
			}
		})
	}

	t.Run("name empty after sanitizing is a 400 naming name", func(t *testing.T) {
		f := putMealFixture(t)
		before := f.meals.Items[0]
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 0, []any{putItem("[[]]\x07\n", "1碗", 480, 20, 70, 12)}, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), meal.FieldName) {
			t.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(before, f.meals.Items[0]) {
			t.Error("a rejected PUT must not change the meal")
		}
	})

	t.Run("untouched items keep their text as stored", func(t *testing.T) {
		f := putMealFixture(t)
		f.meals.EditStored(0, func(m *meal.Meal) {
			m.Items = []meal.FoodItem{
				{Name: "[[記錄]]舊品項", Portion: "1  碗", Nutrients: meal.Nutrients{Calories: 100}, Source: meal.ItemFromAI},
				{Name: "另一個", Nutrients: meal.Nutrients{Calories: 50}},
			}
			m.Recalc()
		})
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", putBody(t, 1, []any{
			putItem("[[記錄]]舊品項", "1  碗", 100, 0, 0, 0), putItem("[[新]]另一個", "", 50, 0, 0, 0),
		}, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		got := f.meals.Items[0].Items
		if got[0].Name != "[[記錄]]舊品項" || got[0].Portion != "1  碗" || got[0].Source != meal.ItemFromAI {
			t.Errorf("an untouched item must be kept byte for byte: %+v", got[0])
		}
		if got[1].Name != "新另一個" {
			t.Errorf("an edited item is sanitized: %+v", got[1])
		}
	})
}

// workoutFixture 一筆 Version 0 的跑步（30 分、5 公里、350 大卡）與體重 70 的身體資料.
func putWorkoutFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPI(t)
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}
	f.workouts.Items = []workout.Workout{{
		ID: "w1", UserID: "U1", Activity: fitness.ActivityRunning, DurationMin: 30, DistanceKM: 5,
		Calories: 350, Source: workout.SourceText, DoneAt: fixedNow.Add(-3 * time.Hour),
	}}
	return f
}

// putWorkoutBody 以「目前值」為底、覆蓋 over 的欄位組出 PUT body；version 為 nil 表示不帶.
func putWorkoutBody(t *testing.T, version any, over map[string]any) string {
	t.Helper()
	m := map[string]any{"activity": fitness.ActivityRunning, "durationMin": 30, "distanceKm": 5, "calories": 350}
	if version != nil {
		m["version"] = version
	}
	for k, v := range over {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPutWorkoutFormulaAndDirectCalories [F34-56] C-9：body.calories 有變用 body 值（不重算）；否則活動／時間／距離有變套公式；
// 全部沒變不寫入、不加 version；活動取正名；範圍檢查；每列 FindByID 讀回.
func TestPutWorkoutFormulaAndDirectCalories(t *testing.T) {
	run := fitness.ActivityByName(fitness.ActivityRunning)
	taichi := fitness.ActivityByName("打太極")
	if formulaCal := fitness.Burned(run, 45, 5, 70); formulaCal == 350 {
		t.Fatalf("precondition: the formula result %v must differ from the stored 350", formulaCal)
	}
	cases := []struct {
		name       string
		over       map[string]any
		wantCal    float64
		wantAct    string
		wantDur    float64
		wantBumped bool
	}{
		{"(1) only calories 350 -> 400", map[string]any{"calories": 400}, 400, fitness.ActivityRunning, 30, true},
		{"(2) only duration 30 -> 45: formula", map[string]any{"durationMin": 45}, fitness.Burned(run, 45, 5, 70), fitness.ActivityRunning, 45, true},
		{"(3) duration and calories: body value, no formula", map[string]any{"durationMin": 45, "calories": 400}, 400, fitness.ActivityRunning, 45, true},
		{"(4) nothing changed: untouched, no version bump", nil, 350, fitness.ActivityRunning, 30, false},
		{"(6) unknown to the table is stored as its canonical name", map[string]any{"activity": "打太極"}, fitness.Burned(taichi, 30, 5, 70), taichi.Name, 30, true},
		{"body-supplied id/userId/doneAt/source are ignored", map[string]any{"id": "w9", "userId": "U2", "doneAt": "2000-01-01T00:00:00Z", "source": "ai", "durationMin": 45}, fitness.Burned(run, 45, 5, 70), fitness.ActivityRunning, 45, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := putWorkoutFixture(t)
			before := f.workouts.Items[0]
			v, ok := putDayRecord(t, f, "workouts")["version"]
			if !ok || v != float64(0) {
				t.Fatalf("a Version-0 record must still carry the version key: version=%v present=%v", v, ok)
			}
			res := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, v, c.over))
			if res.Code != http.StatusOK {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			wantVersion := before.Version
			updates := 0
			if c.wantBumped {
				wantVersion++
				updates = 1
			}
			var got WorkoutView
			_ = json.Unmarshal(res.Body.Bytes(), &got)
			if got.Version != wantVersion || got.ID != "w1" || got.Calories != c.wantCal || got.Activity != c.wantAct || got.DurationMin != c.wantDur {
				t.Errorf("response = %s, want version %d calories %v activity %s duration %v", res.Body.String(), wantVersion, c.wantCal, c.wantAct, c.wantDur)
			}
			stored, err := f.workouts.FindByID(context.Background(), "U1", "w1")
			if err != nil || stored == nil {
				t.Fatalf("FindByID: %v %v", stored, err)
			}
			if stored.Version != wantVersion || stored.Calories != c.wantCal || stored.UserID != "U1" || stored.Source != before.Source || !stored.DoneAt.Equal(before.DoneAt) {
				t.Errorf("stored = %+v", *stored)
			}
			if f.workouts.Updates != updates {
				t.Errorf("Update called %d times, want %d", f.workouts.Updates, updates)
			}
			if !c.wantBumped && !reflect.DeepEqual(before, *stored) {
				t.Errorf("an unchanged PUT must not change the record: %+v", *stored)
			}
		})
	}

	// (5) 舊紀錄存的活動名不是正名：什麼都沒改就存檔不得被判成活動有變——200、版本不變、熱量不重算、活動字串不被改寫.
	t.Run("(5) legacy non-canonical activity name is untouched", func(t *testing.T) {
		const legacy = "瑜伽"
		if fitness.ActivityByName(legacy).Name == legacy {
			t.Fatalf("precondition: %q must normalize to a different canonical name", legacy)
		}
		for _, sent := range []string{legacy, fitness.ActivityByName(legacy).Name} {
			f := putWorkoutFixture(t)
			f.workouts.Items[0].Activity = legacy
			f.workouts.Items[0].Calories = 123 // 刻意不等於公式值：被重算就看得出來
			before := f.workouts.Items[0]
			res := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, 0, map[string]any{"activity": sent, "calories": 123}))
			if res.Code != http.StatusOK {
				t.Fatalf("sent %q: status %d: %s", sent, res.Code, res.Body.String())
			}
			if f.workouts.Updates != 0 || !reflect.DeepEqual(before, f.workouts.Items[0]) {
				t.Errorf("sent %q: stored %+v after %d updates", sent, f.workouts.Items[0], f.workouts.Updates)
			}
		}
	})

	t.Run("no profile: default weight", func(t *testing.T) {
		f := putWorkoutFixture(t)
		delete(f.profiles.Items, "U1")
		res := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, 0, map[string]any{"durationMin": 45}))
		want := fitness.Burned(run, 45, 5, fitness.DefaultWeightKG)
		if res.Code != http.StatusOK || f.workouts.Items[0].Calories != want {
			t.Errorf("status %d, stored calories %v, want %v", res.Code, f.workouts.Items[0].Calories, want)
		}
	})

	rejects := []struct {
		name string
		body func(t *testing.T) string
		code int
	}{
		{"duration 0", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"durationMin": 0}) }, 400},
		{"duration 1441", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"durationMin": 1441}) }, 400},
		{"distance -1", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"distanceKm": -1}) }, 400},
		{"calories 0", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"calories": 0}) }, 400},
		{"calories 10001", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"calories": 10001}) }, 400},
		{"activity blank", func(t *testing.T) string { return putWorkoutBody(t, 0, map[string]any{"activity": "  "}) }, 400},
		{"activity 21 chars", func(t *testing.T) string {
			return putWorkoutBody(t, 0, map[string]any{"activity": strings.Repeat("字", 21)})
		}, 400},
		{"version missing", func(t *testing.T) string { return putWorkoutBody(t, nil, nil) }, 400},
		{"version not a number", func(t *testing.T) string { return putWorkoutBody(t, "abc", nil) }, 400},
		{"not JSON", func(t *testing.T) string { return `not json` }, 400},
		{"stale version", func(t *testing.T) string { return putWorkoutBody(t, 7, map[string]any{"durationMin": 45}) }, 409},
	}
	for _, c := range rejects {
		t.Run("reject/"+c.name, func(t *testing.T) {
			f := putWorkoutFixture(t)
			before := f.workouts.Items[0]
			rec := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", c.body(t))
			if rec.Code != c.code {
				t.Errorf("status %d, want %d: %s", rec.Code, c.code, rec.Body.String())
			}
			if c.code == 409 && !strings.Contains(rec.Body.String(), `"4009"`) {
				t.Errorf("409 must carry the conflict code: %s", rec.Body.String())
			}
			if !reflect.DeepEqual(before, f.workouts.Items[0]) {
				t.Errorf("a rejected PUT must not change the record: %+v", f.workouts.Items[0])
			}
		})
	}
	t.Run("no token", func(t *testing.T) {
		f := putWorkoutFixture(t)
		if rec := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "", putWorkoutBody(t, 0, nil)); rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d", rec.Code)
		}
	})
}

// deleteAfterFind 讀出之後把那筆紀錄刪掉：模擬 FindByID 與 Update 之間帳號資料被清掉.
type deleteAfterFind struct {
	*storagetest.Workouts
}

func (s *deleteAfterFind) FindByID(ctx context.Context, uid, id string) (*workout.Workout, error) {
	w, err := s.Workouts.FindByID(ctx, uid, id)
	if _, derr := s.DeleteByID(ctx, uid, id); derr != nil {
		return nil, derr
	}
	return w, err
}

// TestPutMealNeverRecreatesDeletedRecord [F34-53][F34-56] C-8：不存在的 id、或讀出之後被刪的紀錄，PUT 都是 404（不是 410），store 內沒有該文件（不會被建回）.
func TestPutMealNeverRecreatesDeletedRecord(t *testing.T) {
	t.Run("meal deleted between read and write", func(t *testing.T) {
		f := putMealFixture(t)
		body := putBody(t, 0, []any{putItem("改過", "", 1, 1, 1, 1)}, nil)
		if rec := f.do(http.MethodPut, "/api/v1/me/meals/missing", "good-U1", body); rec.Code != http.StatusNotFound {
			t.Errorf("unknown id: status %d", rec.Code)
		}
		f.meals.BeforeUpdate = func(m *storagetest.Meals) {
			m.BeforeUpdate = nil
			m.Items = nil // 帳號資料被清掉
		}
		rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("deleted meanwhile: status %d (must be 404, not 410): %s", rec.Code, rec.Body.String())
		}
		if len(f.meals.Items) != 0 {
			t.Errorf("a deleted meal must not come back: %+v", f.meals.Items)
		}
		// 對照：鉤子不刪 → 200.
		g := putMealFixture(t)
		if rec := g.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", body); rec.Code != http.StatusOK {
			t.Errorf("control: status %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("workout deleted between read and write", func(t *testing.T) {
		f := putWorkoutFixture(t)
		if rec := f.do(http.MethodPut, "/api/v1/me/workouts/missing", "good-U1", putWorkoutBody(t, 0, nil)); rec.Code != http.StatusNotFound {
			t.Errorf("unknown id: status %d", rec.Code)
		}
		v, _ := newVerifier(t)
		e := echo.New()
		h := NewHandler(f.meals, &deleteAfterFind{Workouts: f.workouts}, f.profiles)
		h.now = func() time.Time { return fixedNow }
		h.Routes(e.Group("/api/v1/me", NoStore, Auth(v)))
		g := &apiFixture{e: e, meals: f.meals, workouts: f.workouts, profiles: f.profiles}
		rec := g.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, 0, map[string]any{"calories": 400}))
		if rec.Code != http.StatusNotFound {
			t.Errorf("deleted meanwhile: status %d (must be 404, not 410): %s", rec.Code, rec.Body.String())
		}
		if len(f.workouts.Items) != 0 {
			t.Errorf("a deleted workout must not come back: %+v", f.workouts.Items)
		}
	})
}

// TestNetBandWordBoundary [F34-57] 詞界比對的兩個輸入：「熱量 0～3000」不命中，獨立的 300 命中（弱化成子字串比對時 3000 那條會紅）.
func TestNetBandWordBoundary(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"熱量 0～3000", false},
		{`var NET_BAND = 300;`, true},
		{`x = [300, 1]`, true},
		{`1300`, false},
	} {
		if got := netBand.MatchString(c.in); got != c.want {
			t.Errorf("netBand.MatchString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestFrontendEditRecords [F34-57] E-1：app.js 的餐點與運動編輯表單（字串冒煙測試；實際點擊行為由 tests/e2e/liff 的無頭瀏覽器驗）.
func TestFrontendEditRecords(t *testing.T) {
	js, _ := frontendSources(t)
	mealForm := jsFuncBody(t, js, "mealEditForm")
	workForm := jsFuncBody(t, js, "workoutEditForm")
	submit := jsFuncBody(t, js, "submitEdit")
	row := jsFuncBody(t, js, "mealEditRow")
	scale := jsFuncBody(t, js, "scaleMacroFields")
	actions := jsFuncBody(t, js, "recordActions")

	// 兩張表單各自組自己的 PUT body，且都把該筆從 /me/day 拿到的 version 原樣帶回.
	mustContainAll(t, "mealEditForm", mealForm, `"/meals/"`, "version: m.version", "submitEdit(")
	mustContainAll(t, "workoutEditForm", workForm, `"/workouts/"`, "version: w.version", "submitEdit(")
	mustContainAll(t, "submitEdit", submit, `method: "PUT"`)
	for _, c := range []struct{ name, body string }{{"mealEditForm", mealForm}, {"workoutEditForm", workForm}} {
		for _, bad := range []string{"userId", "total:", "eatenAt", "doneAt", "scale:"} {
			if strings.Contains(c.body, bad) {
				t.Errorf("%s: the PUT body must not carry %q", c.name, bad)
			}
		}
	}

	// 範圍文字：只對 MEAL_RANGE_TEXT 與三個表單函式判定（全檔有別的 5000：圖表刻度、重載節流）.
	m := regexp.MustCompile(`var MEAL_RANGE_TEXT = "([^"]*)"`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("MEAL_RANGE_TEXT not found in app.js")
	}
	mustContainAll(t, "MEAL_RANGE_TEXT", m[1], "熱量 0～3000", "份量 30 字內")
	for name, src := range map[string]string{"MEAL_RANGE_TEXT": m[1], "mealEditForm": mealForm, "workoutEditForm": workForm, "submitEdit": submit} {
		if strings.Contains(src, "5000") {
			t.Errorf("%s must not mention 5000 (the per-item calorie cap is 3000)", name)
		}
	}
	// 份量欄 maxlength 30（聊天可存 30 字，舊分支的 20 會讓使用者打開自己的舊份量就被截斷）.
	mustContainAll(t, "mealEditRow", row, `["portion", "份量", "text", it.portion, { maxlength: "30" }]`)

	// 欄位標籤.
	mustContainAll(t, "mealEditRow", row, "名稱", "份量", "熱量", "蛋白質", "碳水", "脂肪")
	mustContainAll(t, "mealEditForm", mealForm, "刪除此品項")
	mustContainAll(t, "workoutEditForm", workForm, "活動", "時間", "距離", "熱量")

	// 改熱量時營養素依新舊熱量比例自動帶入.
	mustContainAll(t, "scaleMacroFields", scale, "newCal / row.baseCal", "MACRO_KEYS.forEach")
	mustContainAll(t, "mealEditRow", row, `inputs.calories.addEventListener("input"`, "scaleMacroFields(")

	// 409 沿用「這筆剛在其他地方被修改」並重載；400 / 404 也有處理；已過期的世代不寫畫面.
	mustContainAll(t, "submitEdit", submit, `"http-409"`, "STALE_RECORD_TEXT", "loadAll()", `"http-400"`, `"http-404"`, "gen !== state.gen")
	mustContainAll(t, "app.js", js, "這筆剛在其他地方被修改")

	// 運動表單的公式提示文字.
	mustContainAll(t, "app.js", js, "改時間／距離會依公式重算熱量；直接改熱量則以你填的為準")
	mustContainAll(t, "workoutEditForm", workForm, "WORKOUT_FORMULA_HINT")

	// 刪除鈕必須在每列 DOM 順序的第一顆（tests/e2e/liff/stale.test.mjs 以 `#meal-list li button` 取刪除鈕）；編輯鈕在它之後.
	if !strings.Contains(actions, "box.append(deleteButton(label, onDelete), edit)") {
		t.Errorf("recordActions must append the delete button before the edit button")
	}
	if strings.Contains(js, "innerHTML") {
		t.Error("app.js must not use innerHTML")
	}
}
