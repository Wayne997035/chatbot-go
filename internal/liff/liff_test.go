package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
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

// ---- API ----

type memMeals struct{ meals []meal.Meal }

func (m *memMeals) Insert(_ context.Context, x *meal.Meal) error {
	m.meals = append(m.meals, *x)
	return nil
}

func (m *memMeals) FindByUserBetween(_ context.Context, uid string, from, to time.Time) ([]meal.Meal, error) {
	var out []meal.Meal
	for i := range m.meals {
		if x := m.meals[i]; x.UserID == uid && !x.EatenAt.Before(from) && x.EatenAt.Before(to) {
			out = append(out, x)
		}
	}
	return out, nil
}

func (m *memMeals) DeleteLatest(context.Context, string, time.Time) (*meal.Meal, error) {
	return nil, nil
}

func (m *memMeals) DeleteAllByUser(context.Context, string) error { return nil }

func (m *memMeals) FindByID(_ context.Context, uid, id string) (*meal.Meal, error) {
	for i := range m.meals {
		if m.meals[i].ID == id && m.meals[i].UserID == uid {
			cp := m.meals[i]
			cp.Items = append([]meal.FoodItem(nil), cp.Items...)
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memMeals) Update(_ context.Context, x *meal.Meal) (bool, error) {
	for i := range m.meals {
		if m.meals[i].ID == x.ID && m.meals[i].UserID == x.UserID {
			m.meals[i] = *x
			return true, nil
		}
	}
	return false, nil
}

func (m *memMeals) DeleteByID(_ context.Context, uid, id string) (bool, error) {
	for i := range m.meals {
		if m.meals[i].ID == id && m.meals[i].UserID == uid {
			m.meals = append(m.meals[:i], m.meals[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type memWorkouts struct{ workouts []workout.Workout }

func (m *memWorkouts) Insert(_ context.Context, x *workout.Workout) error {
	m.workouts = append(m.workouts, *x)
	return nil
}

func (m *memWorkouts) FindByUserBetween(_ context.Context, uid string, from, to time.Time) ([]workout.Workout, error) {
	var out []workout.Workout
	for i := range m.workouts {
		if x := m.workouts[i]; x.UserID == uid && !x.DoneAt.Before(from) && x.DoneAt.Before(to) {
			out = append(out, x)
		}
	}
	return out, nil
}

func (m *memWorkouts) DeleteLatest(context.Context, string, time.Time) (*workout.Workout, error) {
	return nil, nil
}

func (m *memWorkouts) DeleteAllByUser(context.Context, string) error { return nil }

func (m *memWorkouts) DeleteByID(_ context.Context, uid, id string) (bool, error) {
	for i := range m.workouts {
		if m.workouts[i].ID == id && m.workouts[i].UserID == uid {
			m.workouts = append(m.workouts[:i], m.workouts[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type memProfiles struct{ p map[string]*profile.Profile }

func (m *memProfiles) Get(_ context.Context, uid string) (*profile.Profile, error) {
	return m.p[uid], nil
}

func (m *memProfiles) Upsert(_ context.Context, p *profile.Profile) error {
	m.p[p.UserID] = p
	return nil
}

func (m *memProfiles) Delete(_ context.Context, uid string) error { delete(m.p, uid); return nil }

type apiFixture struct {
	e        *echo.Echo
	meals    *memMeals
	workouts *memWorkouts
	profiles *memProfiles
}

// now 固定在台灣 2026-11-02 20:00.
var fixedNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	v, _ := newVerifier(t)
	f := &apiFixture{e: echo.New(), meals: &memMeals{}, workouts: &memWorkouts{}, profiles: &memProfiles{p: map[string]*profile.Profile{}}}
	h := NewHandler(f.meals, f.workouts, f.profiles)
	h.now = func() time.Time { return fixedNow }
	me := f.e.Group("/api/v1/me", NoStore, Auth(v))
	me.GET("/summary", h.Summary)
	me.GET("/day", h.Day)
	me.GET("/profile", h.GetProfile)
	me.PUT("/profile", h.PutProfile)
	me.DELETE("/meals/:id", h.DeleteMeal)
	me.DELETE("/workouts/:id", h.DeleteWorkout)
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
	f.meals.meals = []meal.Meal{
		{ID: "m1", UserID: "U1", EatenAt: today, Total: meal.Nutrients{Calories: 600, ProteinG: 30, CarbsG: 70, FatG: 20}},
		{ID: "m2", UserID: "U1", EatenAt: yesterday, Total: meal.Nutrients{Calories: 1500}},
		{ID: "m3", UserID: "U2", EatenAt: today, Total: meal.Nutrients{Calories: 9999}},
	}
	f.workouts.workouts = []workout.Workout{{ID: "w1", UserID: "U1", Activity: "跑步", DistanceKM: 5, Calories: 343, DoneAt: today}}
	f.profiles.p["U1"] = &profile.Profile{UserID: "U1", WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}

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
	f.meals.meals = []meal.Meal{{ID: "mine", UserID: "U1", EatenAt: fixedNow}, {ID: "theirs", UserID: "U2", EatenAt: fixedNow}}
	f.workouts.workouts = []workout.Workout{{ID: "w-theirs", UserID: "U2", DoneAt: fixedNow}}

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
	if len(f.meals.meals) != 1 || f.meals.meals[0].ID != "theirs" {
		t.Errorf("meals = %+v", f.meals.meals)
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
	if p := f.profiles.p["U1"]; p == nil || p.WeightKG != 68 || p.Sex != profile.SexFemale {
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
