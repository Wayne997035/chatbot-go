package liff

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/subscription"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
)

// 統計頁用到的儲存操作（使用端定義的小介面）.
type (
	// MealStore 讀取、改份量（樂觀鎖）與刪除自己的飲食紀錄.
	MealStore interface {
		meal.Editor
		FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]meal.Meal, error)
		DeleteByID(ctx context.Context, userID, id string) (bool, error)
	}
	// WorkoutStore 讀取、修改（樂觀鎖）與刪除自己的運動紀錄.
	WorkoutStore interface {
		FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]workout.Workout, error)
		DeleteByID(ctx context.Context, userID, id string) (bool, error)
		// FindByID [F34-56] 取得使用者自己的一筆；不存在或不屬於該使用者回 nil, nil.
		FindByID(ctx context.Context, userID, id string) (*workout.Workout, error)
		// Update [F34-56] 整筆寫回（CAS，不 upsert）；不存在回 false，版本不符回 workout.ErrConflict.
		Update(ctx context.Context, w *workout.Workout) (bool, error)
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

// profileWriteTimeout [F34-30] PutProfile 寫入個資的期限；沒有期限時慢請求可以拖過關機預算，關機時資料寫入與補刪被硬切.
const profileWriteTimeout = 1 * time.Second

// MaxPutProfileDuration [F34-30] 一個 PUT /me/profile 最長的處理時間：寫入期限 + 複查（erasureOpTimeout）+ 補刪（erasureOpTimeout）.
// 必須小於關機預算（server 的 shutdownTimeout），由 server 套件的測試鎖住.
const MaxPutProfileDuration = profileWriteTimeout + 2*erasureOpTimeout

// recordWriteTimeout [F34-52] 改份量、刪餐點、刪運動與編輯餐點／運動的整段處理期限；沒有期限時慢請求可以拖過關機預算.
// 3 秒的依據：meal.Modify 最壞 3 輪 x（讀 + 寫）= 6 次往返、每次 0.5 秒；沒有寫後複查與補刪，總處理時間就是這一個期限.
const recordWriteTimeout = 3 * time.Second

// MaxRecordWriteDuration [F34-52] 一個紀錄寫入請求最長的處理時間；必須小於關機預算（server 的 shutdownTimeout），由 server 套件的測試鎖住.
const MaxRecordWriteDuration = recordWriteTimeout

// recordCtx [F34-52] 請求 ctx 加上紀錄寫入的期限；呼叫端 MUST defer 回傳的 cancel.
func recordCtx(c *echo.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request().Context(), recordWriteTimeout)
}

// Handler 統計頁的資料 API；每個請求只會讀寫已驗證使用者自己的資料.
type Handler struct {
	meals    MealStore
	workouts WorkoutStore
	profiles ProfileStore
	erased   ErasureChecker // 可為 nil：不檢查
	now      func() time.Time

	// [F30-03] 會員資料與通知訂閱的依賴，一律用 With... 注入（沒注入時對應的路由回 503，不執行）.
	claims       ClaimsVerifier
	users        AccountStore
	eraser       AccountEraser
	subs         subscription.Service
	eraseTimeout time.Duration // R3 整體逾時；零值 = defaultEraseTimeout（測試可調短）
}

// WithErasureCheck [F291-08] 注入刪除標記查詢；未注入或 nil 表示 PUT /me/profile 不做寫後再查.
func (h *Handler) WithErasureCheck(e ErasureChecker) *Handler {
	h.erased = e
	return h
}

// WithClaimsVerifier [F30-03] 注入 R1 用的 claims 驗證器（通常是同一個 *Verifier）.
func (h *Handler) WithClaimsVerifier(v ClaimsVerifier) *Handler {
	h.claims = v
	return h
}

// WithAccountStore [F30-03] 注入會員資料儲存（R1 寫、R2 讀、R1 補刪）.
func (h *Handler) WithAccountStore(s AccountStore) *Handler {
	h.users = s
	return h
}

// WithAccountEraser [F30-04] 注入刪除帳號的實作（與聊天「確認刪除」同一份 EraseAll）.
func (h *Handler) WithAccountEraser(e AccountEraser) *Handler {
	h.eraser = e
	return h
}

// WithSubscription [F30-05] 注入共用訂閱服務（與聊天端同一個）.
func (h *Handler) WithSubscription(s subscription.Service) *Handler {
	h.subs = s
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
func (h *Handler) Summary(c *echo.Context) error {
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
func (h *Handler) Day(c *echo.Context) error {
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
	g.PUT("/meals/:id", h.PutMeal)       // [F34-53]
	g.PUT("/workouts/:id", h.PutWorkout) // [F34-56]
	g.DELETE("/meals/:id", h.DeleteMeal)
	g.DELETE("/workouts/:id", h.DeleteWorkout)
	// [F30-03..07] 會員資料、通知訂閱、縣市鄉鎮清單、整月資料；身分一律是 userID(c)，不讀 query / body / path 的使用者 ID.
	g.POST("/account/sync", h.SyncAccount)
	g.GET("/account", h.GetAccount)
	g.DELETE("/account", h.DeleteAccount)
	g.GET("/alerts", h.ListAlerts)
	g.PUT("/alerts/:type", h.PutAlert)
	g.DELETE("/alerts/:type", h.DeleteAlert)
	g.GET("/regions", h.Regions)
	g.GET("/month", h.Month)
}

// MealScale PATCH /me/meals/:id 的內容.
type MealScale struct {
	// Scale 相對於估算的份量倍數（0.25 ～ 3）.
	Scale float64 `json:"scale"`
}

// ScaleMeal PATCH /me/meals/:id：把自己的一餐改成估算的 scale 倍.
func (h *Handler) ScaleMeal(c *echo.Context) error {
	var in MealScale
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	if !meal.ValidScale(in.Scale) {
		return badRequest(c, "scale must be between 0.25 and 3")
	}
	// 同時在聊天裡加品項或記店家時，在最新的資料上改份量，不會蓋掉對方的修改.
	ctx, cancel := recordCtx(c) // [F34-52]
	defer cancel()
	m, err := meal.Modify(ctx, h.meals, userID(c), c.Param("id"), func(m *meal.Meal) { m.SetScale(in.Scale) })
	switch {
	case errors.Is(err, meal.ErrConflict):
		return conflict(c)
	case err != nil:
		return serverError(c, "update meal", err)
	case m == nil:
		return notFound(c)
	}
	return httputil.OK(c, toMealView(m))
}

// MealItemEdit [F34-53] PUT /me/meals/:id 的一個品項.
type MealItemEdit struct {
	Name     string  `json:"name"`
	Portion  string  `json:"portion"`
	Calories float64 `json:"calories"`
	ProteinG float64 `json:"protein_g"`
	CarbsG   float64 `json:"carbs_g"`
	FatG     float64 `json:"fat_g"`
}

// MealEdit [F34-53] PUT /me/meals/:id 的內容；其他欄位（id、userId、total、scale、eatenAt）一律忽略.
type MealEdit struct {
	// Version 缺省要回 400（指標才分得出缺省與 0）.
	Version *int64         `json:"version"`
	Items   []MealItemEdit `json:"items"`
}

// PutMeal [F34-53] PUT /me/meals/:id：整批取代自己那一餐的品項（只改品項，其他欄位不動），版本不符回 409.
func (h *Handler) PutMeal(c *echo.Context) error {
	var in MealEdit
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	if in.Version == nil {
		return badRequest(c, "version is required")
	}
	ctx, cancel := recordCtx(c) // [F34-52] 讀、驗證、寫共用同一個期限
	defer cancel()
	m, err := h.meals.FindByID(ctx, userID(c), c.Param("id"))
	if err != nil {
		return serverError(c, "load meal", err)
	}
	if m == nil {
		return notFound(c)
	}
	if m.Version != *in.Version {
		return conflict(c)
	}
	items, unchanged := editedItems(m.Items, in.Items)
	var priorCalories float64
	for i := range m.Items {
		priorCalories += m.Items[i].Calories
	}
	if err := meal.ValidateEdit(items, nutrition.ContainsURL, meal.EditContext{
		Unchanged: unchanged, PriorCount: len(m.Items), PriorCalories: priorCalories,
	}); err != nil {
		return badRequest(c, err.Error())
	}
	m.Items = items
	m.Rebase() // [F34-53] 手改後的數值就是新的估算基準；份量倍數歸 ×1
	updated, err := h.meals.Update(ctx, m)
	switch {
	case errors.Is(err, meal.ErrConflict):
		return conflict(c)
	case err != nil:
		return serverError(c, "update meal", err)
	case !updated:
		return notFound(c) // 期間被刪除；Update 不會讓它復活
	}
	return httputil.OK(c, toMealView(m))
}

// editedItems [F34-55] 把送來的品項換成儲存格式，並決定每項的 Source 與是否「沒改」：
//  1. 先配對「名稱、份量、四個營養值全等、尚未被配對」的既有品項（依序，每個既有品項只配一次）：沒改，原樣保留（含 Source、不清理、免範圍檢查）.
//  2. 其餘是有改的：名稱與份量先用聊天路徑同一個 SanitizeDisplay 清理 [F34-54]；再找「名稱與四個營養值相同、尚未被配對」的既有品項沿用其
//     Source（份量不參與，只改份量不算改了來源）；找不到就是 ItemFromUser.
//
// 用內容而不是索引配對，因為刪掉前面的品項會讓後面的索引位移.
func editedItems(existing []meal.FoodItem, in []MealItemEdit) (items []meal.FoodItem, unchanged []bool) {
	used := make([]bool, len(existing))
	items = make([]meal.FoodItem, len(in))
	unchanged = make([]bool, len(in))
	nutrients := func(e *MealItemEdit) meal.Nutrients {
		return meal.Nutrients{Calories: e.Calories, ProteinG: e.ProteinG, CarbsG: e.CarbsG, FatG: e.FatG}
	}
	for i := range in {
		e := &in[i]
		for j := range existing {
			if used[j] || existing[j].Name != e.Name || existing[j].Portion != e.Portion || existing[j].Nutrients != nutrients(e) {
				continue
			}
			used[j], unchanged[i] = true, true
			items[i] = existing[j]
			break
		}
	}
	for i := range in {
		if unchanged[i] {
			continue
		}
		e := &in[i]
		item := meal.FoodItem{
			Name: recordedit.SanitizeDisplay(e.Name, 0), Portion: recordedit.SanitizeDisplay(e.Portion, 0),
			Nutrients: nutrients(e), Source: meal.ItemFromUser,
		}
		for j := range existing {
			if used[j] || existing[j].Name != item.Name || existing[j].Nutrients != item.Nutrients {
				continue
			}
			used[j] = true
			item.Source = existing[j].Source
			break
		}
		items[i] = item
	}
	return items, unchanged
}

func conflict(c *echo.Context) error {
	return c.JSON(http.StatusConflict, httputil.ErrorWithCode(httputil.ErrorCodeConflict, "Record changed, please reload"))
}

// 運動活動名稱最多字數（PUT /me/workouts/:id）.
const maxActivityRunes = 20

// WorkoutEdit [F34-56] PUT /me/workouts/:id 的內容；其他欄位（id、userId、doneAt、source）一律忽略.
type WorkoutEdit struct {
	// Version 缺省要回 400（指標才分得出缺省與 0）.
	Version     *int64  `json:"version"`
	Activity    string  `json:"activity"`
	DurationMin float64 `json:"durationMin"`
	DistanceKM  float64 `json:"distanceKm"`
	Calories    float64 `json:"calories"`
}

// PutWorkout [F34-56] PUT /me/workouts/:id：改活動、時間、距離、熱量.
// 熱量規則：body.calories 與目前值不同 → 用 body.calories（使用者直接改熱量，不重算）；
// 否則活動／時間／距離任一有變 → 以 fitness.Burned 公式重算；全部沒變 → 200 回原值、不寫入、不加 version.
func (h *Handler) PutWorkout(c *echo.Context) error {
	var in WorkoutEdit
	if err := c.Bind(&in); err != nil {
		return badRequest(c, "invalid body")
	}
	if in.Version == nil {
		return badRequest(c, "version is required")
	}
	ctx, cancel := recordCtx(c) // [F34-52] 讀、查身體資料、寫共用同一個期限
	defer cancel()
	w, err := h.workouts.FindByID(ctx, userID(c), c.Param("id"))
	if err != nil {
		return serverError(c, "load workout", err)
	}
	if w == nil {
		return notFound(c)
	}
	if w.Version != *in.Version {
		return conflict(c)
	}
	if msg := workoutEditError(&in); msg != "" {
		return badRequest(c, msg)
	}
	next := *w
	next.Activity = editedActivity(w.Activity, &in)
	next.DurationMin, next.DistanceKM = in.DurationMin, in.DistanceKM
	calories, changed, err := h.editedCalories(ctx, userID(c), w, &next, &in)
	if err != nil {
		return serverError(c, "load profile", err)
	}
	if !changed {
		return httputil.OK(c, toWorkoutView(w)) // 全部沒變
	}
	next.Calories = calories
	if next.Calories <= 0 || !fitness.Plausible(next.DurationMin, next.DistanceKM, next.Calories) {
		return badRequest(c, "calories must be > 0, durationMin <= 1440, distanceKm <= 500, calories <= 10000")
	}
	updated, err := h.workouts.Update(ctx, &next)
	switch {
	case errors.Is(err, workout.ErrConflict):
		return conflict(c)
	case err != nil:
		return serverError(c, "update workout", err)
	case !updated:
		return notFound(c) // 期間被刪除；Update 不會讓它復活
	}
	return httputil.OK(c, toWorkoutView(&next))
}

// editedCalories [F34-56] 熱量怎麼決定：body.calories 有變 → 用 body 值；否則活動／時間／距離有變 → 公式重算（體重取身體資料，沒有就用預設）；
// 都沒變 → changed 為 false.
func (h *Handler) editedCalories(
	ctx context.Context, uid string, w, next *workout.Workout, in *WorkoutEdit,
) (calories float64, changed bool, err error) {
	if in.Calories != w.Calories {
		return in.Calories, true, nil
	}
	if next.Activity == w.Activity && next.DurationMin == w.DurationMin && next.DistanceKM == w.DistanceKM {
		return w.Calories, false, nil
	}
	p, err := h.profiles.Get(ctx, uid)
	if err != nil {
		return 0, false, err
	}
	weight := 0.0 // Burned 對 0 以預設體重計算
	if p != nil {
		weight = p.WeightKG
	}
	return fitness.Burned(fitness.ActivityByName(next.Activity), next.DurationMin, next.DistanceKM, weight), true, nil
}

// workoutEditError [F34-56] 欄位範圍檢查（活動名稱長度、時間 > 0、距離 >= 0）；上限由 fitness.Plausible 在熱量決定後一併檢查. 沒問題回空字串.
func workoutEditError(in *WorkoutEdit) string {
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Activity)); n < 1 || n > maxActivityRunes {
		return fmt.Sprintf("activity: need 1 to %d characters", maxActivityRunes)
	}
	if math.IsNaN(in.DurationMin) || in.DurationMin <= 0 || math.IsNaN(in.DistanceKM) || in.DistanceKM < 0 || math.IsNaN(in.Calories) {
		return "durationMin must be > 0 and distanceKm must be >= 0"
	}
	return ""
}

// editedActivity [F34-56] 活動名稱與儲存的字面相同、或取正名後相同，就原樣保留（儲存的可能是舊的非正名，不能因此被當成有變）；否則存正名，不認識的存「其他運動」.
func editedActivity(current string, in *WorkoutEdit) string {
	name := strings.TrimSpace(in.Activity)
	canonical := fitness.ActivityByName(name).Name
	if name == current || canonical == fitness.ActivityByName(current).Name {
		return current // 字面相同，或取正名後相同：沒換活動，不改寫舊值
	}
	return canonical
}

// DeleteMeal DELETE /me/meals/:id.
func (h *Handler) DeleteMeal(c *echo.Context) error {
	return deleteRecord(c, h.meals.DeleteByID)
}

// DeleteWorkout DELETE /me/workouts/:id.
func (h *Handler) DeleteWorkout(c *echo.Context) error {
	return deleteRecord(c, h.workouts.DeleteByID)
}

// deleteRecord [F34-52] 刪除自己的一筆紀錄；餐點與運動共用，整段處理走同一個寫入期限.
func deleteRecord(c *echo.Context, del func(ctx context.Context, userID, id string) (bool, error)) error {
	ctx, cancel := recordCtx(c)
	defer cancel()
	ok, err := del(ctx, userID(c), c.Param("id"))
	return deleted(c, ok, err)
}

// GetProfile GET /me/profile.
func (h *Handler) GetProfile(c *echo.Context) error {
	p, err := h.profiles.Get(c.Request().Context(), userID(c))
	if err != nil {
		return serverError(c, "load profile", err)
	}
	return httputil.OK(c, toProfileView(p))
}

// PutProfile PUT /me/profile.
func (h *Handler) PutProfile(c *echo.Context) error {
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
	// [F34-30] 寫入加期限；期限到時 Upsert 回錯，仍照舊進複查與補刪（可能已落地）.
	wctx, wcancel := context.WithTimeout(c.Request().Context(), profileWriteTimeout)
	upsertErr := h.profiles.Upsert(wctx, p)
	wcancel()
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

func deleted(c *echo.Context, ok bool, err error) error {
	if err != nil {
		return serverError(c, "delete record", err)
	}
	if !ok {
		return notFound(c)
	}
	return c.NoContent(http.StatusNoContent)
}

func notFound(c *echo.Context) error {
	return c.JSON(http.StatusNotFound, httputil.ErrorWithCode(httputil.ErrorCodeNotFound, "Record not found"))
}

func badRequest(c *echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(httputil.ErrorCodeInvalidParameter, msg))
}

func serverError(c *echo.Context, what string, err error) error {
	if !errors.Is(err, c.Request().Context().Err()) {
		// [F30-08] 儲存錯誤可能夾帶 userID、email 或主機位址.
		slog.Error("LIFF API", "op", what, "error", redact.Error(err))
	}
	return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(httputil.ErrorCodeServerError, "Please try again later"))
}
