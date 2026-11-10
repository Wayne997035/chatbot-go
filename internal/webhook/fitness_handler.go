package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"fmt"
	"log/slog"
	"time"
)

// 運動與統計關鍵字.
const (
	keywordLogWorkout    = "記運動"
	keywordToday         = "今日統計"
	keywordWeek          = "本週統計"
	keywordDeleteWorkout = "刪除上一筆運動"
	keywordProfile       = "身體資料"
	keywordDeleteData    = "刪除我的資料"
	keywordConfirm       = "確認"
	keywordCancel        = "取消"
)

var (
	todayAliases   = map[string]bool{keywordToday: true, "今天消耗": true, "熱量收支": true, "今日熱量": true}
	weekAliases    = map[string]bool{keywordWeek: true, "週統計": true, "這週統計": true, "運動紀錄": true}
	profileAliases = map[string]bool{keywordProfile: true, "我的資料": true, "設定身體資料": true}
	confirmWords   = map[string]bool{keywordConfirm: true, "確認記錄": true, "確認刪除": true, "好": true, "對": true, "是": true, "ok": true}
)

// WithFitness 啟用運動紀錄；aiParser 為 nil 時只用規則解析.
func (h *WebhookHandler) WithFitness(
	workoutRepo WorkoutStore, profileRepo ProfileStore, aiParser *fitness.AIParser,
) *WebhookHandler {
	h.workoutRepo = workoutRepo
	h.profileRepo = profileRepo
	h.aiParser = aiParser
	return h
}

// WithAlertHistory [F241-11] 設定警報推播紀錄的清除者；刪除資料時一併清掉.
func (h *WebhookHandler) WithAlertHistory(e AlertHistoryEraser) *WebhookHandler {
	h.alertHistory = e
	return h
}

// WithLIFF 設定 LIFF 統計頁網址；統計回覆會多一個「看圖表」按鈕.
func (h *WebhookHandler) WithLIFF(url string) *WebhookHandler {
	h.liffURL = url
	return h
}

// withChartButton 有 LIFF 時在最前面加「看圖表」.
func (h *WebhookHandler) withChartButton(items []models.QuickReplyItem) []models.QuickReplyItem {
	if h.liffURL == "" {
		return items
	}
	return append([]models.QuickReplyItem{models.URIItem("看圖表", h.liffURL)}, items...)
}

func statsQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.MessageItem("今日統計", keywordToday),
		models.MessageItem("本週統計", keywordWeek),
		models.MessageItem("記運動", keywordLogWorkout),
		models.CameraItem("拍食物"),
	}
}

func activityQuickReplies() []models.QuickReplyItem {
	items := make([]models.QuickReplyItem, 0, len(fitness.QuickActivities))
	for _, a := range fitness.QuickActivities {
		items = append(items, models.MessageItem(a, a))
	}
	return items
}

func durationQuickReplies(activity string) []models.QuickReplyItem {
	items := []models.QuickReplyItem{}
	if activity == fitness.ActivityRunning || activity == fitness.ActivityWalking {
		items = append(items, models.MessageItem("3 公里", "3公里"), models.MessageItem("5 公里", "5公里"))
	}
	for _, m := range []int{15, 30, 45, 60, 90} {
		label := fmt.Sprintf("%d 分鐘", m)
		items = append(items, models.MessageItem(label, fmt.Sprintf("%d分鐘", m)))
	}
	return append(items, models.MessageItem("取消", keywordCancel))
}

func confirmQuickReplies(confirmText string) []models.QuickReplyItem {
	return []models.QuickReplyItem{models.MessageItem("✅ "+confirmText, keywordConfirm), models.MessageItem("❌ 取消", keywordCancel)}
}

// handleFitnessKeyword 處理運動、統計、身體資料相關文字；回傳 true 表示已處理.
func (h *WebhookHandler) handleFitnessKeyword(ctx context.Context, userID, text, replyToken string) bool {
	if h.workoutRepo == nil {
		return false
	}
	switch {
	case text == keywordLogWorkout:
		_ = h.line.ReplyText(ctx, replyToken, "做了什麼運動？點選或直接輸入，例如「跑步 5公里 30分鐘」「重訓 1小時」", activityQuickReplies()...)
	case todayAliases[text]:
		h.replyTodayStats(ctx, userID, replyToken)
	case weekAliases[text]:
		h.replyWeekStats(ctx, userID, replyToken)
	case text == keywordDeleteWorkout:
		h.deleteLatestWorkout(ctx, userID, replyToken)
	case profileAliases[text]:
		h.replyProfile(ctx, userID, replyToken)
	case text == keywordDeleteData:
		h.askDeleteData(ctx, userID, replyToken)
	default:
		return h.handleWorkoutText(ctx, userID, text, replyToken)
	}
	return true
}

// handleWorkoutText 規則解析身體資料或運動描述.
func (h *WebhookHandler) handleWorkoutText(ctx context.Context, userID, text, replyToken string) bool {
	if update, ok := fitness.ParseProfile(text); ok {
		h.updateProfile(ctx, userID, replyToken, &update)
		return true
	}
	parsed := fitness.Parse(text)
	if parsed.Activity == nil {
		return false
	}
	if parsed.HasAmount() {
		h.recordWorkout(ctx, userID, replyToken, parsed, workout.SourceText)
		return true
	}
	if fitness.IsActivityOnly(text) {
		h.askDuration(ctx, userID, replyToken, parsed.Activity.Name)
		return true
	}
	return false
}

// handleFitnessConversation 處理運動相關的多輪對話；回傳 true 表示已處理.
func (h *WebhookHandler) handleFitnessConversation(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) bool {
	switch state.Type {
	case conversation.TypeWorkoutDuration:
		h.handlePendingDuration(ctx, userID, text, replyToken, state)
	case conversation.TypeWorkoutConfirm:
		h.handlePendingConfirm(ctx, userID, text, replyToken, state)
	case conversation.TypeDeleteDataConfirm:
		h.handleDeleteDataConfirm(ctx, userID, text, replyToken)
	default:
		return false
	}
	return true
}

func (h *WebhookHandler) askDuration(ctx context.Context, userID, replyToken, activity string) {
	if !h.convManager.Available() {
		_ = h.line.ReplyText(ctx, replyToken, "請一次輸入運動與時間，例如「"+activity+" 30分鐘」")
		return
	}
	state := &conversation.State{Type: conversation.TypeWorkoutDuration, Workout: &conversation.PendingWorkout{Activity: activity}}
	err := h.convManager.Set(ctx, userID, state)
	// [F291-03] 寫入之後才發現使用者已刪除資料：整份對話狀態清掉（寫入回錯也複查，可能已落地）.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if err != nil {
		slog.Error("set workout state", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "請一次輸入運動與時間，例如「"+activity+" 30分鐘」")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, activity+"多久呢？", durationQuickReplies(activity)...)
}

func (h *WebhookHandler) handlePendingDuration(ctx context.Context, userID, text, replyToken string, state *conversation.State) {
	if text == keywordCancel {
		h.clearState(ctx, userID)
		_ = h.line.ReplyText(ctx, replyToken, "已取消")
		return
	}
	parsed := fitness.Parse(text)
	if !parsed.HasAmount() {
		_ = h.line.ReplyText(ctx, replyToken, "請輸入時間或距離，例如「30分鐘」「1小時」「5公里」", durationQuickReplies(state.Workout.Activity)...)
		return
	}
	h.clearState(ctx, userID)
	if parsed.Activity == nil {
		parsed.Activity = fitness.ActivityByName(state.Workout.Activity)
	}
	h.recordWorkout(ctx, userID, replyToken, parsed, workout.SourceText)
}

func (h *WebhookHandler) handlePendingConfirm(ctx context.Context, userID, text, replyToken string, state *conversation.State) {
	h.clearState(ctx, userID)
	if !confirmWords[text] || state.Workout == nil {
		_ = h.line.ReplyText(ctx, replyToken, "已取消，沒有記錄", activityQuickReplies()...)
		return
	}
	p := state.Workout
	h.recordWorkout(ctx, userID, replyToken, &fitness.Parsed{
		Activity: fitness.ActivityByName(p.Activity), DurationMin: p.DurationMin, DistanceKM: p.DistanceKM, Calories: p.Calories,
	}, workout.SourceAI)
}

// tryAIWorkout 規則看不懂、但像在描述運動的文字交給 AI，解析結果先請使用者確認.
func (h *WebhookHandler) tryAIWorkout(ctx context.Context, userID, text, replyToken string) bool {
	if h.aiParser == nil || h.workoutRepo == nil || !h.convManager.Available() || !fitness.LooksLikeWorkout(text) {
		return false
	}
	parsed, ok := h.aiParser.Parse(ctx, text)
	if !ok {
		return false
	}
	w := &workout.Workout{Activity: parsed.Activity.Name, DurationMin: parsed.DurationMin, DistanceKM: parsed.DistanceKM}
	state := &conversation.State{Type: conversation.TypeWorkoutConfirm, Workout: &conversation.PendingWorkout{
		Activity: parsed.Activity.Name, DurationMin: parsed.DurationMin, DistanceKM: parsed.DistanceKM, Calories: parsed.Calories,
	}}
	err := h.convManager.Set(ctx, userID, state)
	// [F291-03] 同上：確認狀態含 AI 解析出的運動，刪除後落地要清掉，並回已刪除而不是要不要記錄.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return true
	}
	if err != nil {
		slog.Error("set workout confirm state", "error", err)
		return false
	}
	_ = h.line.ReplyText(ctx, replyToken, "要記錄這筆運動嗎？\n"+fitness.Describe(w), confirmQuickReplies("記錄")...)
	return true
}

// implausibleWorkoutMessage 數值不合理時的回覆.
const implausibleWorkoutMessage = "這筆數字看起來不太對（時間超過一天、距離超過 500 公里或熱量超過 10000 大卡），請再確認一次"

// recordWorkout 計算熱量、存檔並回覆.
func (h *WebhookHandler) recordWorkout(ctx context.Context, userID, replyToken string, p *fitness.Parsed, source string) {
	weight := h.weightOf(ctx, userID)
	duration := p.DurationMin
	if duration == 0 {
		duration = fitness.EstimateDuration(p.Activity, p.DistanceKM)
	}
	calories := p.Calories
	fromApp := calories > 0
	if !fromApp {
		calories = fitness.Burned(p.Activity, duration, p.DistanceKM, weight)
	}
	if calories <= 0 {
		h.askDuration(ctx, userID, replyToken, p.Activity.Name)
		return
	}
	if !fitness.Plausible(duration, p.DistanceKM, calories) {
		_ = h.line.ReplyText(ctx, replyToken, implausibleWorkoutMessage, activityQuickReplies()...)
		return
	}
	now := time.Now()
	w := &workout.Workout{
		UserID: userID, Activity: p.Activity.Name, DurationMin: duration, DistanceKM: p.DistanceKM,
		Calories: calories, Source: source, DoneAt: now, CreateTime: now,
	}
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	err := h.workoutRepo.Insert(ctx, w)
	// [F291-04] 寫後再查（回錯也查）：只補刪這次寫的那筆（Insert 在寫入前就設好 ID），刪除後新送的運動不受影響.
	if h.guardAfterWrite(ctx, userID, replyToken, func(cctx context.Context) error {
		_, delErr := h.workoutRepo.DeleteByID(cctx, userID, w.ID)
		return delErr
	}) {
		return
	}
	if err != nil {
		slog.Error("save workout", "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, "記錄失敗，請稍後再試")
		return
	}
	todayBurned := calories
	if workouts, err := h.todayWorkouts(ctx, userID); err == nil {
		todayBurned = 0
		for i := range workouts {
			todayBurned += workouts[i].Calories
		}
	}
	_ = h.line.ReplyText(ctx, replyToken, fitness.FormatRecorded(w, weight, todayBurned, fromApp), append(statsQuickReplies()[:2],
		models.MessageItem("刪除這筆", keywordDeleteWorkout))...)
}

// recordScreenshot 存下運動 App 截圖讀出的紀錄.
func (h *WebhookHandler) recordScreenshot(ctx context.Context, userID, replyToken string, r *nutrition.ExerciseReading) {
	if h.workoutRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, "運動紀錄功能目前未啟用")
		return
	}
	h.recordWorkout(ctx, userID, replyToken, &fitness.Parsed{
		Activity: fitness.ActivityByName(r.Activity), DurationMin: r.DurationMin, DistanceKM: r.DistanceKM, Calories: r.Calories,
	}, workout.SourceScreenshot)
}

func (h *WebhookHandler) weightOf(ctx context.Context, userID string) float64 {
	if h.profileRepo == nil {
		return 0
	}
	p, err := h.profileRepo.Get(ctx, userID)
	if err != nil || p == nil {
		return 0
	}
	return p.WeightKG
}

func (h *WebhookHandler) todayWorkouts(ctx context.Context, userID string) ([]workout.Workout, error) {
	from, to := nutrition.DayRange(time.Now())
	return h.workoutRepo.FindByUserBetween(ctx, userID, from, to)
}

func (h *WebhookHandler) replyTodayStats(ctx context.Context, userID, replyToken string) {
	from, to := nutrition.DayRange(time.Now())
	meals, workouts, err := h.loadPeriod(ctx, userID, from, to)
	if err != nil {
		slog.Error("load today stats", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "讀取統計失敗，請稍後再試")
		return
	}
	var base float64
	if h.profileRepo != nil {
		if p, err := h.profileRepo.Get(ctx, userID); err == nil {
			base = fitness.DailyBase(p)
		}
	}
	_ = h.line.ReplyText(ctx, replyToken, fitness.FormatToday(meals, workouts, base), h.withChartButton(statsQuickReplies()[1:])...)
}

const weekDays = 7

func (h *WebhookHandler) replyWeekStats(ctx context.Context, userID, replyToken string) {
	todayStart, tomorrow := nutrition.DayRange(time.Now())
	days := make([]time.Time, weekDays)
	for i := range days {
		days[i] = todayStart.AddDate(0, 0, i-(weekDays-1))
	}
	meals, workouts, err := h.loadPeriod(ctx, userID, days[0], tomorrow)
	if err != nil {
		slog.Error("load week stats", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "讀取統計失敗，請稍後再試")
		return
	}
	week := fitness.FormatWeek(fitness.Summarize(days, meals, workouts))
	_ = h.line.ReplyText(ctx, replyToken, week, h.withChartButton(statsQuickReplies())...)
}

func (h *WebhookHandler) loadPeriod(ctx context.Context, userID string, from, to time.Time) ([]meal.Meal, []workout.Workout, error) {
	var meals []meal.Meal
	if h.mealRepo != nil {
		m, err := h.mealRepo.FindByUserBetween(ctx, userID, from, to)
		if err != nil {
			return nil, nil, err
		}
		meals = m
	}
	workouts, err := h.workoutRepo.FindByUserBetween(ctx, userID, from, to)
	if err != nil {
		return nil, nil, err
	}
	return meals, workouts, nil
}

func (h *WebhookHandler) deleteLatestWorkout(ctx context.Context, userID, replyToken string) {
	deleted, err := h.workoutRepo.DeleteLatest(ctx, userID, time.Now().Add(-deleteWindow))
	switch {
	case err != nil:
		slog.Error("delete latest workout", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "刪除失敗，請稍後再試")
	case deleted == nil:
		_ = h.line.ReplyText(ctx, replyToken, "24 小時內沒有可以刪除的運動紀錄")
	default:
		_ = h.line.ReplyText(ctx, replyToken, "已刪除："+fitness.Describe(deleted), statsQuickReplies()[:3]...)
	}
}

func (h *WebhookHandler) replyProfile(ctx context.Context, userID, replyToken string) {
	var p *profile.Profile
	if h.profileRepo != nil {
		got, err := h.profileRepo.Get(ctx, userID)
		if err != nil {
			slog.Error("get profile", "error", err)
		}
		p = got
	}
	_ = h.line.ReplyText(ctx, replyToken, fitness.FormatProfile(p))
}

func (h *WebhookHandler) updateProfile(ctx context.Context, userID, replyToken string, update *fitness.ProfileUpdate) {
	if h.profileRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, "身體資料功能目前未啟用")
		return
	}
	// 只更新這次提到的欄位（原子操作），連續傳的幾則設定同時處理也不會互相覆蓋.
	patch := &profile.Profile{UserID: userID, UpdateTime: time.Now()}
	update.Apply(patch)
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	p, err := h.profileRepo.Merge(ctx, patch)
	// [F291-05] 身體資料是單一文件，無法只刪這次的欄位，補刪整份（刪除後的窗口內重送一次即可）.
	if h.guardAfterWrite(ctx, userID, replyToken, func(cctx context.Context) error {
		return h.profileRepo.Delete(cctx, userID)
	}) {
		return
	}
	if err != nil {
		slog.Error("save profile", "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, "儲存失敗，請稍後再試")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "已更新！\n"+fitness.FormatProfile(p), statsQuickReplies()[:3]...)
}

func (h *WebhookHandler) askDeleteData(ctx context.Context, userID, replyToken string) {
	if !h.convManager.Available() {
		_ = h.line.ReplyText(ctx, replyToken, "目前無法處理，請稍後再試")
		return
	}
	err := h.convManager.Set(ctx, userID, &conversation.State{Type: conversation.TypeDeleteDataConfirm})
	// [F291-03] 刪除之後殘留的「確認刪除」狀態，會讓一則無關的「確認」再觸發刪除；寫入回錯（刪除確認必須存共享儲存）也複查.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if err != nil {
		slog.Error("set delete confirm state", "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, "目前無法處理，請稍後再試")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "確定要刪除你所有的個人資料嗎？包含飲食紀錄、運動紀錄、身體資料、問答紀錄、記住的地點，"+
		"以及天氣特報與地震通知的訂閱。刪除後無法復原。", confirmQuickReplies("確認刪除")...)
}

func (h *WebhookHandler) handleDeleteDataConfirm(ctx context.Context, userID, text, replyToken string) {
	h.clearState(ctx, userID)
	if !confirmWords[text] {
		_ = h.line.ReplyText(ctx, replyToken, "已取消，資料都還在")
		return
	}
	// 先記下刪除時間：刪除前送出、還在處理中的訊息之後就不能把資料寫回去.
	// 標記寫不進共享儲存時資料照刪，但回刪除失敗：其他實例看不到標記，不能說已刪除；使用者重新走一次刪除流程會重寫標記
	// （上面已清掉確認狀態，單獨再按「確認刪除」不會觸發刪除）.
	markErr := h.erased.mark(ctx, userID)
	failed := h.eraseUserData(ctx, userID)
	if markErr != nil || len(failed) > 0 {
		_ = h.line.ReplyText(ctx, replyToken, eraseFailedMessage)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "已刪除你的所有個人資料：飲食紀錄、運動紀錄、身體資料、問答紀錄、記住的地點和通知訂閱。")
}

// eraseUserData 刪除使用者的所有個人資料；每一部分都會嘗試，回傳失敗的部分.
func (h *WebhookHandler) eraseUserData(ctx context.Context, userID string) []string {
	var failed []string
	attempt := func(part string, err error) {
		if err != nil {
			failed = append(failed, part)
			slog.Error("delete user data", "part", part, "error", err)
		}
	}
	if h.mealRepo != nil {
		attempt("meals", h.mealRepo.DeleteAllByUser(ctx, userID))
	}
	if h.workoutRepo != nil {
		attempt("workouts", h.workoutRepo.DeleteAllByUser(ctx, userID))
	}
	if h.profileRepo != nil {
		attempt("profile", h.profileRepo.Delete(ctx, userID))
	}
	if h.alertSubRepo != nil {
		attempt("alert subscriptions", h.alertSubRepo.DeleteByUser(ctx, userID))
	}
	// [F241-11] 排在刪訂閱之後：下一輪警報就讀不到這個人，再清掉已寫下的推播紀錄.
	if h.alertHistory != nil {
		attempt("alert history", h.alertHistory.ForgetRecipient(ctx, userID))
	}
	attempt("qa history", h.assistant.Forget(ctx, userID))
	attempt("locations", h.locations.Forget(ctx, userID))
	attempt("recent meal", h.recent.Forget(ctx, userID))
	// [F241-10] eraseUserData 在 mark 之後才執行，這裡清掉 clearState 與 mark 之間寫入的對話狀態
	attempt("conversation", h.convManager.Delete(ctx, userID))
	if h.userRepo != nil {
		attempt("user", h.userRepo.Delete(ctx, userID))
	}
	return failed
}

// clearDeleteConfirm [F24-06] 非文字訊息打斷「確認刪除」：只清刪除確認狀態，其他狀態（等店家位置、等補照片）保留.
func (h *WebhookHandler) clearDeleteConfirm(ctx context.Context, userID string) {
	state, err := h.convManager.GetStrict(ctx, userID)
	if err != nil {
		// [F24-63] 讀不到就無法確認是不是刪除確認：保守地直接清掉，避免舊確認在非文字訊息後仍有效
		slog.Warn("read conversation state for delete confirm failed, clearing it", "error", redact.Error(err))
		h.clearState(ctx, userID)
		return
	}
	if state != nil && state.Type == conversation.TypeDeleteDataConfirm {
		h.clearState(ctx, userID)
	}
}

func (h *WebhookHandler) clearState(ctx context.Context, userID string) {
	if err := h.convManager.Delete(ctx, userID); err != nil {
		slog.Error("clear conversation state", "error", err)
	}
}
