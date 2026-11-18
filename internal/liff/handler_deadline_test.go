package liff

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// blockingProfiles [F34-30] Upsert 阻塞到 ctx 結束，或 3 秒到（保險，避免沒有期限時整個套件卡死）；記下收到的 ctx 狀態.
type blockingProfiles struct {
	mu          sync.Mutex
	hasDeadline bool
	remaining   time.Duration
}

func (b *blockingProfiles) Get(context.Context, string) (*profile.Profile, error) { return nil, nil }

func (b *blockingProfiles) Upsert(ctx context.Context, _ *profile.Profile) error {
	b.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		b.hasDeadline, b.remaining = true, time.Until(dl)
	}
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return nil
	}
}

func (b *blockingProfiles) Delete(context.Context, string) error { return nil }

// TestPutProfileUpsertHasDeadline [F34-30] PutProfile 的 Upsert 收到有期限的 ctx（<=1 秒），慢寫入在 2 秒內就回應.
func TestPutProfileUpsertHasDeadline(t *testing.T) {
	bp := &blockingProfiles{}
	api := newProfileAPI(t, bp, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- putProfile(t, context.Background(), api) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 when the write deadline expires: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PutProfile did not respond within 2s: Upsert ctx has no deadline")
	}

	bp.mu.Lock()
	defer bp.mu.Unlock()
	if !bp.hasDeadline {
		t.Fatal("Upsert ctx has no deadline")
	}
	if bp.remaining <= 0 || bp.remaining > time.Second {
		t.Errorf("Upsert ctx deadline remaining = %v, want in (0, 1s]", bp.remaining)
	}
}

// deadlineProbe [F34-52] 記下 store 收到的 ctx 是否有期限、剩多久；block 阻塞到 ctx 結束，或 8 秒到（保險，避免沒有期限時整個套件卡死）.
type deadlineProbe struct {
	mu          sync.Mutex
	hasDeadline bool
	remaining   time.Duration
}

func (p *deadlineProbe) block(ctx context.Context) error {
	p.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		p.hasDeadline, p.remaining = true, time.Until(dl)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
		return nil
	}
}

// probeMeals 指定的那一個方法阻塞（"find" / "update" / "delete"），其他照常.
type probeMeals struct {
	*storagetest.Meals
	probe *deadlineProbe
	block string
}

func (s *probeMeals) FindByID(ctx context.Context, uid, id string) (*meal.Meal, error) {
	if s.block == "find" {
		if err := s.probe.block(ctx); err != nil {
			return nil, err
		}
	}
	return s.Meals.FindByID(ctx, uid, id)
}

func (s *probeMeals) Update(ctx context.Context, m *meal.Meal) (bool, error) {
	if s.block == "update" {
		if err := s.probe.block(ctx); err != nil {
			return false, err
		}
	}
	return s.Meals.Update(ctx, m)
}

func (s *probeMeals) DeleteByID(ctx context.Context, uid, id string) (bool, error) {
	if s.block == "delete" {
		if err := s.probe.block(ctx); err != nil {
			return false, err
		}
	}
	return s.Meals.DeleteByID(ctx, uid, id)
}

type probeWorkouts struct {
	*storagetest.Workouts
	probe *deadlineProbe
	block string
}

func (s *probeWorkouts) FindByID(ctx context.Context, uid, id string) (*workout.Workout, error) {
	if s.block == "find" {
		if err := s.probe.block(ctx); err != nil {
			return nil, err
		}
	}
	return s.Workouts.FindByID(ctx, uid, id)
}

func (s *probeWorkouts) Update(ctx context.Context, w *workout.Workout) (bool, error) {
	if s.block == "update" {
		if err := s.probe.block(ctx); err != nil {
			return false, err
		}
	}
	return s.Workouts.Update(ctx, w)
}

func (s *probeWorkouts) DeleteByID(ctx context.Context, uid, id string) (bool, error) {
	if s.block == "delete" {
		if err := s.probe.block(ctx); err != nil {
			return false, err
		}
	}
	return s.Workouts.DeleteByID(ctx, uid, id)
}

// recordRequest 送一個帶驗證的請求到 handler（回應放進 channel，讓呼叫端用 select 等）.
func recordRequest(e *echo.Echo, method, path, body string) chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer good-U1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

// TestRecordWritesHaveDeadline [F34-52] 每個紀錄寫入 handler 都把有期限（<= 3 秒）的 ctx 交給 store，慢儲存在 5 秒內回 500；
// 控制組：同一個 handler 接不阻塞的 store 回成功，證明 500 來自期限而不是誤殺.
func TestRecordWritesHaveDeadline(t *testing.T) {
	stored := func() []meal.Meal {
		items := []meal.FoodItem{{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650, ProteinG: 32, CarbsG: 80, FatG: 20}}}
		return []meal.Meal{{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients}}
	}
	rows := []deadlineRow{
		{"PATCH meal find", http.MethodPatch, "/api/v1/me/meals/m1", `{"scale":0.5}`, "meal", "find", http.StatusOK, http.StatusInternalServerError},
		{"PATCH meal update", http.MethodPatch, "/api/v1/me/meals/m1", `{"scale":0.5}`, "meal", "update", http.StatusOK, http.StatusInternalServerError},
		{"DELETE meal", http.MethodDelete, "/api/v1/me/meals/m1", "", "meal", "delete", http.StatusNoContent, http.StatusInternalServerError},
		{"DELETE workout", http.MethodDelete, "/api/v1/me/workouts/w1", "", "workout", "delete", http.StatusNoContent, http.StatusInternalServerError},
	}
	rows = append(rows, putDeadlineRows()...)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			build := func(block string) (*echo.Echo, *deadlineProbe) {
				probe := &deadlineProbe{}
				v, _ := newVerifier(t)
				pm := &probeMeals{Meals: &storagetest.Meals{Items: stored()}, probe: probe}
				pw := &probeWorkouts{
					Workouts: &storagetest.Workouts{Items: []workout.Workout{{ID: "w1", UserID: "U1", Activity: "跑步", DurationMin: 30, DistanceKM: 5, Calories: 350, DoneAt: fixedNow}}},
					probe:    probe,
				}
				if r.store == "meal" {
					pm.block = block
				} else {
					pw.block = block
				}
				e := echo.New()
				h := NewHandler(pm, pw, storagetest.NewProfiles())
				h.now = func() time.Time { return fixedNow }
				h.Routes(e.Group("/api/v1/me", NoStore, Auth(v)))
				return e, probe
			}

			e, _ := build("")
			select {
			case rec := <-recordRequest(e, r.method, r.path, r.body):
				if rec.Code != r.wantOK {
					t.Fatalf("control (store not blocking): status = %d, want %d: %s", rec.Code, r.wantOK, rec.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("control (store not blocking) did not respond within 5s")
			}

			e, probe := build(r.block)
			select {
			case rec := <-recordRequest(e, r.method, r.path, r.body):
				if rec.Code != r.wantStatus {
					t.Errorf("status = %d, want %d when the write deadline expires: %s", rec.Code, r.wantStatus, rec.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not respond within 5s: the store ctx has no deadline")
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if !probe.hasDeadline {
				t.Fatal("store ctx has no deadline")
			}
			if probe.remaining <= 0 || probe.remaining > 3*time.Second {
				t.Errorf("store ctx deadline remaining = %v, want in (0, 3s]", probe.remaining)
			}
		})
	}
}

// deadlineRow [F34-52] TestRecordWritesHaveDeadline 的一列.
type deadlineRow struct {
	name       string
	method     string
	path       string
	body       string
	store      string // "meal" 或 "workout"
	block      string // 哪個 store 方法阻塞："find" / "update" / "delete"
	wantOK     int    // 控制組（不阻塞）的狀態碼
	wantStatus int
}

// putDeadlineRows 兩個 PUT handler 的列（FindByID 阻塞與 Update 阻塞各一列）.
func putDeadlineRows() []deadlineRow {
	return nil
}
