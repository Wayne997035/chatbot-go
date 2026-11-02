package liff

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

// Handler 統計頁的資料 API；每個請求只會讀寫已驗證使用者自己的資料.
type Handler struct {
	meals    meal.MealRepository
	workouts workout.WorkoutRepository
	profiles profile.ProfileRepository
	now      func() time.Time
}

// NewHandler 建立 API 處理器.
func NewHandler(meals meal.MealRepository, workouts workout.WorkoutRepository, profiles profile.ProfileRepository) *Handler {
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
	Profile *ProfileView   `json:"profile"`
	Base    float64        `json:"base"`
	Today   TodayView      `json:"today"`
	Days    []DayView      `json:"days"`
	Totals  meal.Nutrients `json:"totals"`
}

// TodayView 今日摘要.
type TodayView struct {
	Intake float64        `json:"intake"`
	Burned float64        `json:"burned"`
	Macros meal.Nutrients `json:"macros"`
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

	view := SummaryView{Base: fitness.DailyBase(p), Profile: toProfileView(p), Totals: meal.Sum(meals)}
	for _, s := range fitness.Summarize(starts, meals, workouts) {
		view.Days = append(view.Days, DayView{
			Date: s.Date.Format(dateLayout), Intake: s.Intake, Meals: s.Meals, Burned: s.Burned, Workouts: s.Workouts, RunKM: s.RunKM,
		})
	}
	for i := range meals {
		if !meals[i].EatenAt.Before(todayStart) {
			view.Today.Macros.Add(&meals[i].Total)
		}
	}
	last := view.Days[len(view.Days)-1]
	view.Today.Intake, view.Today.Burned = last.Intake, last.Burned
	return httputil.OK(c, view)
}

// DayRecords 某一天的明細.
type DayRecords struct {
	Date     string            `json:"date"`
	Meals    []meal.Meal       `json:"meals"`
	Workouts []workout.Workout `json:"workouts"`
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
	if meals == nil {
		meals = []meal.Meal{}
	}
	if workouts == nil {
		workouts = []workout.Workout{}
	}
	return httputil.OK(c, DayRecords{Date: from.Format(dateLayout), Meals: meals, Workouts: workouts})
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
	var in ProfileView
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	p := &profile.Profile{UserID: userID(c), WeightKG: in.WeightKG, HeightCM: in.HeightCM, Age: in.Age, Sex: in.Sex, UpdateTime: h.now()}
	if err := fitness.ValidateProfile(p); err != nil {
		return badRequest(c, err.Error())
	}
	if err := h.profiles.Upsert(c.Request().Context(), p); err != nil {
		return serverError(c, "save profile", err)
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
		return c.JSON(http.StatusNotFound, httputil.ErrorWithCode(httputil.ErrorCodeNotFound, "Record not found"))
	}
	return c.NoContent(http.StatusNoContent)
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
