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
	"regexp"
	"slices"
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
		"韋恩紅茶", "LINE 顯示名稱", "頭像", "email", "身體資料", "飲食與運動紀錄", "餐點照片與文字",
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

// TestFrontendMemberAndDeleteAccount [F30-21] 會員資料與刪除帳號：不用 innerHTML、頭像限 https、email 空顯示未提供、成功後才清同意.
func TestFrontendMemberAndDeleteAccount(t *testing.T) {
	js, html := frontendSources(t)
	render := jsFuncBody(t, js, "renderAccount")
	mustContainAll(t, "renderAccount", render, `"未提供"`, `isHttpsUrl(a.pictureUrl)`, `.textContent = name`, `.textContent = email`, "avatar-initial", `"error"`)
	if strings.Contains(render, "innerHTML") {
		t.Error("renderAccount must use textContent only")
	}
	if !strings.Contains(jsFuncBody(t, js, "isHttpsUrl"), `/^https:\/\//i`) {
		t.Error("avatar URL must be restricted to https")
	}
	mustContainAll(t, "index.html", html, "刪除我的帳號資料", `id="delete-cancel"`, `id="delete-ok"`, "確定刪除", "無法復原", "LINE 帳號本身不受影響")
	// 預設焦點在「取消」；刪除確認不得用裸 confirm().
	mustContainAll(t, "app.js", js, `getElementById("delete-cancel").focus()`, `api("/account", { method: "DELETE" })`, "部分資料刪除失敗，請稍後再試一次", "刪除失敗，請稍後再試")
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

// TestFrontendCalendarTab [F30-23] 月曆：週日為第一欄、grade 白名單對應 class、前端不判級.
func TestFrontendCalendarTab(t *testing.T) {
	js, html := frontendSources(t)
	mustContainAll(t, "index.html", html, `data-tab="calendar"`, `id="cal-grid"`, `id="month-prev"`, `id="month-next"`)
	mustContainAll(t, "app.js", js, `"/month?month="`, "getUTCDay()", `{ green: "grade-green", yellow: "grade-yellow", red: "grade-red" }`,
		"hasOwnProperty.call(GRADE_CLASS, d.grade)", `"▲ "`, "aria-label", "state.month >= todayString().slice(0, 7)")
	// 前端不寫門檻：沒有 300，也不對淨值做分級比較（門檻只在 Go 常數）.
	for _, bad := range []string{"300", "d.intake - ", "net <=", "net >="} {
		cal := jsFuncBody(t, js, "calendarCell") + jsFuncBody(t, js, "renderMonth")
		if strings.Contains(cal, bad) {
			t.Errorf("calendar code must not compute grade (found %q)", bad)
		}
	}
	if strings.Contains(js, "300") {
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
