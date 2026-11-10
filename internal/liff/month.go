package liff

import (
	"math"
	"time"

	"chatbot-go/internal/fitness"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/nutrition"

	"github.com/labstack/echo/v4"
)

// NetBandKcal [F30-07] 月曆三級上色的門檻（kcal，單一定義；日後調整只改這裡）：
// 淨值 ≤ -NetBandKcal 綠、-NetBandKcal ~ +NetBandKcal 黃、> +NetBandKcal 紅.
const NetBandKcal = 300

// 月曆每日等級.
const (
	gradeGreen  = "green"
	gradeYellow = "yellow"
	gradeRed    = "red"
)

const (
	monthLayout = "2006-01"
	// firstMonth 月曆可查的最早月份.
	firstMonthYear = 2020
)

// MonthDay 月曆的一天：同 summary 的每日欄位，加上三級 grade（DayView 本身不動，summary.json / day.json 不變）.
type MonthDay struct {
	DayView
	// Grade "green" | "yellow" | "red" | ""（沒身體資料或當天沒有飲食紀錄時為 ""）.
	Grade string `json:"grade"`
}

// MonthView R6 的回應.
type MonthView struct {
	Month string     `json:"month"`
	Base  float64    `json:"base"`
	Days  []MonthDay `json:"days"`
}

// netGrade [F30-07] 淨值三級：net = round(intake - base - burned)，先取整數再比；base == 0（身體資料未設）或當天沒有飲食紀錄 → "".
func netGrade(intake, base, burned float64, meals int) string {
	if base == 0 || meals == 0 {
		return ""
	}
	net := math.Round(intake - base - burned)
	switch {
	case net <= -NetBandKcal:
		return gradeGreen
	case net <= NetBandKcal:
		return gradeYellow
	default:
		return gradeRed
	}
}

// Month GET /me/month?month=YYYY-MM：該月每一天的彙總與三級 grade（F30-07）.
func (h *Handler) Month(c echo.Context) error {
	monthStart, err := time.ParseInLocation(monthLayout, c.QueryParam("month"), nutrition.Taipei)
	if err != nil {
		return badRequest(c, "month must be YYYY-MM")
	}
	nowTW := h.now().In(nutrition.Taipei)
	currentMonth := time.Date(nowTW.Year(), nowTW.Month(), 1, 0, 0, 0, 0, nutrition.Taipei)
	if monthStart.Year() < firstMonthYear || monthStart.After(currentMonth) {
		return badRequest(c, "month out of range")
	}
	next := monthStart.AddDate(0, 1, 0)
	days := int(next.Sub(monthStart).Hours() / 24)
	starts := make([]time.Time, days)
	for i := range starts {
		starts[i] = monthStart.AddDate(0, 0, i)
	}

	uid := userID(c)
	ctx := c.Request().Context()
	meals, err := h.meals.FindByUserBetween(ctx, uid, monthStart, next)
	if err != nil {
		return serverError(c, "load meals", err)
	}
	workouts, err := h.workouts.FindByUserBetween(ctx, uid, monthStart, next)
	if err != nil {
		return serverError(c, "load workouts", err)
	}
	p, err := h.profiles.Get(ctx, uid)
	if err != nil {
		return serverError(c, "load profile", err)
	}

	view := MonthView{Month: monthStart.Format(monthLayout), Base: fitness.DailyBase(p), Days: make([]MonthDay, 0, days)}
	for _, s := range fitness.Summarize(starts, meals, workouts) {
		view.Days = append(view.Days, MonthDay{
			DayView: DayView{
				Date: s.Date.Format(dateLayout), Intake: s.Intake, Meals: s.Meals, Burned: s.Burned, Workouts: s.Workouts, RunKM: s.RunKM,
			},
			Grade: netGrade(s.Intake, view.Base, s.Burned, s.Meals),
		})
	}
	return httputil.OK(c, view)
}
