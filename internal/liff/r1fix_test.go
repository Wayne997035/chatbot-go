package liff

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v5"
)

// countingMeals 記錄 FindByID 被呼叫幾次（證明請求在讀取儲存之前就被擋下）.
type countingMeals struct {
	*storagetest.Meals
	finds atomic.Int32
}

func (c *countingMeals) FindByID(ctx context.Context, userID, id string) (*meal.Meal, error) {
	c.finds.Add(1)
	return c.Meals.FindByID(ctx, userID, id)
}

type countingWorkouts struct {
	*storagetest.Workouts
	finds atomic.Int32
}

func (c *countingWorkouts) FindByID(ctx context.Context, userID, id string) (*workout.Workout, error) {
	c.finds.Add(1)
	return c.Workouts.FindByID(ctx, userID, id)
}

// putThroughRoutes 以經過 Routes 與 Auth 的真實路由送 PUT，儲存層是計數包裝.
type putThroughRoutes struct {
	e        *echo.Echo
	meals    *countingMeals
	workouts *countingWorkouts
}

func newPutThroughRoutes(t *testing.T) *putThroughRoutes {
	t.Helper()
	v, _ := newVerifier(t)
	f := &putThroughRoutes{
		e:        echo.New(),
		meals:    &countingMeals{Meals: &storagetest.Meals{}},
		workouts: &countingWorkouts{Workouts: &storagetest.Workouts{}},
	}
	h := NewHandler(f.meals, f.workouts, storagetest.NewProfiles())
	h.Routes(f.e.Group("/api/v1/me", NoStore, Auth(v)))
	return f
}

func (f *putThroughRoutes) put(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-U1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	return rec
}

// TestPutRoutesHaveTheirOwnBodyLimit [F34-53][F34-56] SEC34-01：兩個新 PUT 路由各有自己的小 body 上限，
// 超過就 413，且 handler 沒有讀過儲存（放大發生在 Bind，在 FindByID 之前）.
func TestPutRoutesHaveTheirOwnBodyLimit(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		body  string
		want  int
		finds func(f *putThroughRoutes) int32
	}{
		{
			name: "meal: ~1MB of empty items is cut off before the store is read",
			path: "/api/v1/me/meals/nope",
			body: `{"version":1,"items":[` + strings.Repeat(`{},`, 350_000) + `{}]}`,
			want: http.StatusRequestEntityTooLarge,
			finds: func(f *putThroughRoutes) int32 {
				return f.meals.finds.Load()
			},
		},
		{
			name: "workout: 5KB body is cut off before the store is read",
			path: "/api/v1/me/workouts/nope",
			body: `{"version":1,"activity":"` + strings.Repeat("x", 5<<10) + `"}`,
			want: http.StatusRequestEntityTooLarge,
			finds: func(f *putThroughRoutes) int32 {
				return f.workouts.finds.Load()
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPutThroughRoutes(t)
			rec := f.put(c.path, c.body)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if n := c.finds(f); n != 0 {
				t.Errorf("FindByID calls = %d, want 0 (the body must be rejected before the store is read)", n)
			}
		})
	}
}

// TestPutMealWithinTheBodyLimitStillWorks [F34-53] SEC34-01：20 品項、名稱與份量各 30 字的合法 body 在上限內，不是 413.
func TestPutMealWithinTheBodyLimitStillWorks(t *testing.T) {
	f := putMealFixture(t)
	var items []any
	for range 20 {
		items = append(items, putItem(strings.Repeat("名", 30), strings.Repeat("份", 30), 100, 5, 10, 3))
	}
	body := putBody(t, 0, items, nil)
	if len(body) >= maxMealEditBodyBytes {
		t.Fatalf("precondition: the 20-item body is %d bytes, must be under the %d limit", len(body), maxMealEditBodyBytes)
	}
	rec := f.do(http.MethodPut, "/api/v1/me/meals/m1", "good-U1", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestPutWorkoutWithinTheBodyLimitStillWorks [F34-56] SEC34-01：正常的運動 body 照常.
func TestPutWorkoutWithinTheBodyLimitStillWorks(t *testing.T) {
	f := putWorkoutFixture(t)
	rec := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, 0, map[string]any{"calories": 400}))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// legacyZeroDurationFixture 舊的運動紀錄：只填了熱量 300，時間與距離都是 0.
func legacyZeroDurationFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := putWorkoutFixture(t)
	f.workouts.Items[0].DurationMin = 0
	f.workouts.Items[0].DistanceKM = 0
	f.workouts.Items[0].Calories = 300
	return f
}

// TestPutWorkoutLegacyZeroDurationStaysEditable [F34-56] FE34-R1-01：時間 0 的舊紀錄，只改熱量或只改活動要存得回去，
// 沒改的時間 0 不能讓儲存失敗，也不能被公式把熱量改成 0.
func TestPutWorkoutLegacyZeroDurationStaysEditable(t *testing.T) {
	cases := []struct {
		name    string
		over    map[string]any
		wantCal float64
		wantAct string
	}{
		{"only calories 300 -> 250", map[string]any{"durationMin": 0, "distanceKm": 0, "calories": 250}, 250, "跑步"},
		{"only activity changes: calories stay 300, no formula", map[string]any{"durationMin": 0, "distanceKm": 0, "calories": 300, "activity": "打太極"}, 300, fitness.ActivityByName("打太極").Name},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := legacyZeroDurationFixture(t)
			before := f.workouts.Items[0]
			res := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, before.Version, c.over))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", res.Code, res.Body.String())
			}
			var got WorkoutView
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Calories != c.wantCal || got.DurationMin != 0 || got.Version != before.Version+1 || got.Activity != c.wantAct {
				t.Errorf("response = %s, want calories %v duration 0 version %d activity %s", res.Body.String(), c.wantCal, before.Version+1, c.wantAct)
			}
			stored := f.workouts.Items[0]
			if stored.Calories != c.wantCal || stored.DurationMin != 0 || stored.Version != before.Version+1 {
				t.Errorf("stored = %+v", stored)
			}
		})
	}
}

// TestPutWorkoutNonZeroDurationCannotBecomeZero [F34-56] FE34-R1-01：改前時間 > 0 的紀錄把時間改成 0 照舊 400（只放行「舊的 0 維持 0」）.
func TestPutWorkoutNonZeroDurationCannotBecomeZero(t *testing.T) {
	f := putWorkoutFixture(t) // 時間 30
	before := f.workouts.Items[0]
	res := f.do(http.MethodPut, "/api/v1/me/workouts/w1", "good-U1", putWorkoutBody(t, before.Version, map[string]any{"durationMin": 0}))
	if res.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", res.Code, res.Body.String())
	}
	if f.workouts.Items[0] != before {
		t.Errorf("a rejected edit must not change the record: %+v", f.workouts.Items[0])
	}
}
