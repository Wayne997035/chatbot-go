package liff

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"chatbot-go/internal/reqid"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/dblog"
	"chatbot-go/internal/storage/storagetest"
)

// ctxMeals 記下 FindByID 與 Update 收到的 ctx（PATCH 一次請求會依序各呼叫一次）.
type ctxMeals struct {
	*storagetest.Meals
	mu   sync.Mutex
	ctxs []context.Context
}

func (s *ctxMeals) FindByID(ctx context.Context, uid, id string) (*meal.Meal, error) {
	s.mu.Lock()
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	return s.Meals.FindByID(ctx, uid, id)
}

func (s *ctxMeals) Update(ctx context.Context, m *meal.Meal) (bool, error) {
	s.mu.Lock()
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	return s.Meals.Update(ctx, m)
}

func (s *ctxMeals) reset() {
	s.mu.Lock()
	s.ctxs = nil
	s.mu.Unlock()
}

func newReqIDEnv(t *testing.T) (*echo.Echo, *ctxMeals) {
	t.Helper()
	items := []meal.FoodItem{{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}}}
	store := &ctxMeals{Meals: &storagetest.Meals{Items: []meal.Meal{{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients}}}}
	v, _ := newVerifier(t)
	e := echo.New()
	h := NewHandler(store, &storagetest.Workouts{}, storagetest.NewProfiles())
	h.now = func() time.Time { return fixedNow }
	h.Routes(e.Group("/api/v1/me", NoStore, Auth(v)))
	return e, store
}

func patchMeal(e *echo.Echo) int {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/api/v1/me/meals/m1", strings.NewReader(`{"scale":0.5}`))
	req.Header.Set("Authorization", "Bearer good-U1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

// [F37-08] 同一個請求內兩次寫入取到相同的 reqid.From(ctx)，兩個請求不同.
func TestLiffRequestsCarryOneReqIDEach(t *testing.T) {
	e, store := newReqIDEnv(t)

	if code := patchMeal(e); code != http.StatusOK {
		t.Fatalf("first PATCH status = %d, want 200", code)
	}
	if len(store.ctxs) != 2 {
		t.Fatalf("store saw %d ctxs, want 2 (find + update)", len(store.ctxs))
	}
	a1, ok1 := reqid.From(store.ctxs[0])
	a2, ok2 := reqid.From(store.ctxs[1])
	if !ok1 || !ok2 || a1 != a2 {
		t.Fatalf("within one request: ids = %q(%v), %q(%v), want the same non-empty id", a1, ok1, a2, ok2)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a1) {
		t.Errorf("request id %q is not 16 lower-case hex", a1)
	}

	store.reset()
	if code := patchMeal(e); code != http.StatusOK {
		t.Fatalf("second PATCH status = %d, want 200", code)
	}
	b1, _ := reqid.From(store.ctxs[0])
	if b1 == "" || b1 == a1 {
		t.Errorf("second request id = %q, first = %q; want two different ids", b1, a1)
	}
}

// [F37-08] 請求 ctx 配 dblog.Write：寫入 log 行帶出同一個 req，且不含 userId.
func TestLiffDBWriteLogCarriesRequestID(t *testing.T) {
	e, store := newReqIDEnv(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if code := patchMeal(e); code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", code)
	}
	want, ok := reqid.From(store.ctxs[1])
	if !ok {
		t.Fatal("update ctx has no request id")
	}

	dblog.Write(store.ctxs[1], "meal", dblog.ActionReplace, nil)

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"req":"`+want+`"`) {
		t.Errorf("db write line lacks req %q: %s", want, line)
	}
	if strings.Contains(line, "U1") {
		t.Errorf("db write line leaks the user id: %s", line)
	}
}
