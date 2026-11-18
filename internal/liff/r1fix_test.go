package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
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
