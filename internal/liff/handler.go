package liff

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

// 統計頁用到的儲存操作（使用端定義的小介面）.
type (
	// MealStore 讀取、改份量（樂觀鎖）與刪除自己的飲食紀錄.
	MealStore interface {
		meal.Editor
		FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]meal.Meal, error)
		DeleteByID(ctx context.Context, userID, id string) (bool, error)
	}
	// WorkoutStore 讀取與刪除自己的運動紀錄.
	WorkoutStore interface {
		FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]workout.Workout, error)
		DeleteByID(ctx context.Context, userID, id string) (bool, error)
	}
	// ProfileStore 讀取與整筆儲存身體資料（表單一次送出所有欄位）.
	ProfileStore interface {
		Get(ctx context.Context, userID string) (*profile.Profile, error)
		Upsert(ctx context.Context, p *profile.Profile) error
		// Delete [F291-08] 儲存後才發現使用者已刪除資料時補刪整份.
		Delete(ctx context.Context, userID string) error
	}
	// ErasureChecker [F291-08] 刪除時間不早於 since 嗎（同一毫秒算刪除過；刪除標記由 webhook 擁有，這裡借用，同 alert 套件的接法）.
	ErasureChecker interface {
		ErasedSince(ctx context.Context, userID string, since time.Time) bool
	}
)

// erasureOpTimeout [F291-08] 複查與補刪的期限；不跟著請求 ctx 走，請求結束或取消時補刪仍要做完.
const erasureOpTimeout = 5 * time.Second

// Handler 統計頁的資料 API；每個請求只會讀寫已驗證使用者自己的資料.
type Handler struct {
	meals    MealStore
	workouts WorkoutStore
	profiles ProfileStore
	erased   ErasureChecker // 可為 nil：不檢查
	now      func() time.Time
}

// WithErasureCheck [F291-08] 注入刪除標記查詢；未注入或 nil 表示 PUT /me/profile 不做寫後再查.
func (h *Handler) WithErasureCheck(e ErasureChecker) *Handler {
	h.erased = e
	return h
}

// NewHandler 建立 API 處理器.
func NewHandler(meals MealStore, workouts WorkoutStore, profiles ProfileStore) *Handler {
	return &Handler{meals: meals, workouts: workouts, profiles: profiles, now: time.Now}
}

// 查詢範圍.
const (
	defaultDays = 7
	maxDays     = 90
	dateLayout  = "2006-01-02"
)

// DayView 一天的彙總.
type DayView struct {
	Date     string  `json:"date"`
	Intake   float64 `json:"intake"`
	Meals    int     `json:"meals"`
	Burned   float64 `json:"burned"`
	Workouts int     `json:"workouts"`
	RunKM    float64 `json:"runKm"`
}

// SummaryView 統計頁首頁資料.
type SummaryView struct {
	Profile *ProfileView  `json:"profile"`
	Base    float64       `json:"base"`
	Today   TodayView     `json:"today"`
	Days    []DayView     `json:"days"`
	Totals  NutrientsView `json:"totals"`
}

// TodayView 今日摘要.
type TodayView struct {
	Intake float64       `json:"intake"`
	Burned float64       `json:"burned"`
	Macros NutrientsView `json:"macros"`
}

// ProfileView 身體資料.
type ProfileView struct {
	WeightKG float64 `json:"weightKg"`
	HeightCM float64 `json:"heightCm"`
	Age      int     `json:"age"`
	Sex      string  `json:"sex"`
}

// Summary GET /me/summary?days=7.
func (h *Handler) Summary(c echo.Context) error {
	days := defaultDays
	if v, err := strconv.Atoi(c.QueryParam("days")); err == nil && v > 0 {
		days = min(v, maxDays)
	}
	uid := userID(c)
	todayStart, tomorrow := nutrition.DayRange(h.now())
	starts := make([]time.Time, days)
	for i := range starts {
		starts[i] = todayStart.AddDate(0, 0, i-(days-1))
	}
	meals, err := h.meals.FindByUserBetween(c.Request().Context(), uid, starts[0], tomorrow)
	if err != nil {
		return serverError(c, "load meals", err)
	}
	workouts, err := h.workouts.FindByUserBetween(c.Request().Context(), uid, starts[0], tomorrow)
	if err != nil {
		return serverError(c, "load workouts", err)
	}
	p, err := h.profiles.Get(c.Request().Context(), uid)
	if err != nil {
		return serverError(c, "load profile", err)
	}

	totals := meal.Sum(meals)
	view := SummaryView{Base: fitness.DailyBase(p), Profile: toProfileView(p), Totals: toNutrientsView(&totals)}
	for _, s := range fitness.Summarize(starts, meals, workouts) {
		view.Days = append(view.Days, DayView{
			Date: s.Date.Format(dateLayout), Intake: s.Intake, Meals: s.Meals, Burned: s.Burned, Workouts: s.Workouts, RunKM: s.RunKM,
		})
	}
	var macros meal.Nutrients
	for i := range meals {
		if !meals[i].EatenAt.Before(todayStart) {
			macros.Add(&meals[i].Total)
		}
	}
	view.Today.Macros = toNutrientsView(&macros)
	last := view.Days[len(view.Days)-1]
	view.Today.Intake, view.Today.Burned = last.Intake, last.Burned
	return httputil.OK(c, view)
}

// DayRecords 某一天的明細.
type DayRecords struct {
	Date     string        `json:"date"`
	Meals    []MealView    `json:"meals"`
	Workouts []WorkoutView `json:"workouts"`
}

// Day GET /me/day?date=2026-11-02（台灣日期，預設今天）.
func (h *Handler) Day(c echo.Context) error {
	day := h.now()
	if v := c.QueryParam("date"); v != "" {
		parsed, err := time.ParseInLocation(dateLayout, v, nutrition.Taipei)
		if err != nil {
			return badRequest(c, "date must be YYYY-MM-DD")
		}
		day = parsed
	}
	from, to := nutrition.DayRange(day)
	uid := userID(c)
	meals, err := h.meals.FindByUserBetween(c.Request().Context(), uid, from, to)
	if err != nil {
		return serverError(c, "load meals", err)
	}
	workouts, err := h.workouts.FindByUserBetween(c.Request().Context(), uid, from, to)
	if err != nil {
		return serverError(c, "load workouts", err)
	}
	return httputil.OK(c, DayRecords{Date: from.Format(dateLayout), Meals: toMealViews(meals), Workouts: toWorkoutViews(workouts)})
}

// Routes 註冊 /me 底下的 API；g 必須已經套用 Auth（身分只來自驗證過的 ID token）.
func (h *Handler) Routes(g *echo.Group) {
	g.GET("/summary", h.Summary)
	g.GET("/day", h.Day)
	g.GET("/profile", h.GetProfile)
	g.PUT("/profile", h.PutProfile)
	g.PATCH("/meals/:id", h.ScaleMeal)
	g.DELETE("/meals/:id", h.DeleteMeal)
	g.DELETE("/workouts/:id", h.DeleteWorkout)
}

// MealScale PATCH /me/meals/:id 的內容.
type MealScale struct {
	// Scale 相對於估算的份量倍數（0.25 ～ 3）.
	Scale float64 `json:"scale"`
}

// ScaleMeal PATCH /me/meals/:id：把自己的一餐改成估算的 scale 倍.
func (h *Handler) ScaleMeal(c echo.Context) error {
	var in MealScale
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	if !meal.ValidScale(in.Scale) {
		return badRequest(c, "scale must be between 0.25 and 3")
	}
	// 同時在聊天裡加品項或記店家時，在最新的資料上改份量，不會蓋掉對方的修改.
	m, err := meal.Modify(c.Request().Context(), h.meals, userID(c), c.Param("id"), func(m *meal.Meal) { m.SetScale(in.Scale) })
	switch {
	case errors.Is(err, meal.ErrConflict):
		return c.JSON(http.StatusConflict, httputil.ErrorWithCode(httputil.ErrorCodeConflict, "Record changed, please reload"))
	case err != nil:
		return serverError(c, "update meal", err)
	case m == nil:
		return notFound(c)
	}
	return httputil.OK(c, toMealView(m))
}

// DeleteMeal DELETE /me/meals/:id.
func (h *Handler) DeleteMeal(c echo.Context) error {
	ok, err := h.meals.DeleteByID(c.Request().Context(), userID(c), c.Param("id"))
	return deleted(c, ok, err)
}

// DeleteWorkout DELETE /me/workouts/:id.
func (h *Handler) DeleteWorkout(c echo.Context) error {
	ok, err := h.workouts.DeleteByID(c.Request().Context(), userID(c), c.Param("id"))
	return deleted(c, ok, err)
}

// GetProfile GET /me/profile.
func (h *Handler) GetProfile(c echo.Context) error {
	p, err := h.profiles.Get(c.Request().Context(), userID(c))
	if err != nil {
		return serverError(c, "load profile", err)
	}
	return httputil.OK(c, toProfileView(p))
}

// PutProfile PUT /me/profile.
func (h *Handler) PutProfile(c echo.Context) error {
	// [F291-08] 收到請求的時間（毫秒，早於 Bind 與驗證）：標記不早於它 = 使用者在這次儲存送出之後刪除了資料.
	since := h.now().Truncate(time.Millisecond)
	var in ProfileView
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	p := &profile.Profile{UserID: userID(c), WeightKG: in.WeightKG, HeightCM: in.HeightCM, Age: in.Age, Sex: in.Sex, UpdateTime: h.now()}
	if err := fitness.ValidateProfile(p); err != nil {
		return badRequest(c, err.Error())
	}
	upsertErr := h.profiles.Upsert(c.Request().Context(), p)
	// [F291-08] 寫後再查（回錯也查，可能已落地）：命中就補刪整份並回 410，不能讓刪除前送出的儲存把資料寫回去.
	if h.erased != nil {
		detached := context.WithoutCancel(c.Request().Context())
		rctx, rcancel := context.WithTimeout(detached, erasureOpTimeout)
		erased := h.erased.ErasedSince(rctx, p.UserID, since)
		rcancel()
		if erased {
			// [R2-S4][F293-04] 命中之後才開補刪的期限：讀標記花掉的時間不從補刪扣掉.
			cctx, cancel := context.WithTimeout(detached, erasureOpTimeout)
			defer cancel()
			if err := h.profiles.Delete(cctx, p.UserID); err != nil {
				// [R2-S1][F293-03] 補刪失敗不能宣稱已刪除：回 500 與專屬錯誤碼，前端照實說刪除沒做成；log 遮蔽 userID.
				slog.Error("LIFF API", "op", "undo profile after erasure", "error", redact.Error(err))
				return c.JSON(http.StatusInternalServerError,
					httputil.ErrorWithCode(httputil.ErrorCodeEraseIncomplete, "Data erasure incomplete, please try again"))
			}
			return c.JSON(http.StatusGone, httputil.ErrorWithCode(httputil.ErrorCodeDataErased, "Data was erased, nothing saved"))
		}
	}
	if upsertErr != nil {
		return serverError(c, "save profile", upsertErr)
	}
	return httputil.OK(c, toProfileView(p))
}

func toProfileView(p *profile.Profile) *ProfileView {
	if p == nil {
		return &ProfileView{}
	}
	return &ProfileView{WeightKG: p.WeightKG, HeightCM: p.HeightCM, Age: p.Age, Sex: p.Sex}
}

func deleted(c echo.Context, ok bool, err error) error {
	if err != nil {
		return serverError(c, "delete record", err)
	}
	if !ok {
		return notFound(c)
	}
	return c.NoContent(http.StatusNoContent)
}

func notFound(c echo.Context) error {
	return c.JSON(http.StatusNotFound, httputil.ErrorWithCode(httputil.ErrorCodeNotFound, "Record not found"))
}

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(httputil.ErrorCodeInvalidParameter, msg))
}

func serverError(c echo.Context, what string, err error) error {
	if !errors.Is(err, c.Request().Context().Err()) {
		slog.Error("LIFF API", "op", what, "error", err)
	}
	return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(httputil.ErrorCodeServerError, "Please try again later"))
}
