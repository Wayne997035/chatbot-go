package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/recentmeal"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// 食物照片相關設定.
const (
	keywordTodayMeals = "今日飲食"
	keywordDeleteMeal = "刪除上一筆"
	keywordSnapFood   = "拍食物"

	// photoDailyLimit 每人每天可分析的照片數，避免免費 AI 額度被少數人用光.
	photoDailyLimit = 30
	// imageAnalysisTimeout 看圖分析的時間上限，要在 LINE reply token 失效前回覆.
	imageAnalysisTimeout = 40 * time.Second
	// loadingSeconds 「輸入中」動畫秒數（LINE 限 5 的倍數，最多 60）.
	loadingSeconds = 40
	// deleteWindow 「刪除上一筆」可刪除的時間範圍.
	deleteWindow = 24 * time.Hour
	// photoMergeWindow 打字記餐後多久內傳的照片會併到同一餐（而不是新增一筆）.
	photoMergeWindow = 10 * time.Minute
)

var todayMealAliases = map[string]bool{keywordTodayMeals: true, "今天吃什麼": true, "今天吃了什麼": true, "飲食紀錄": true}

// WithNutrition 啟用食物照片分析與打字記餐；analyzer 為 nil 表示 AI 未啟用. mealRepo、limiter 可為 nil.
func (h *WebhookHandler) WithNutrition(analyzer *nutrition.Analyzer, mealRepo meal.MealRepository, limiter *redis.Client) *WebhookHandler {
	h.nutrition = analyzer
	h.mealRepo = mealRepo
	h.limiter = limiter
	h.recent = recentmeal.New(limiter)
	h.erased = newErasures(limiter)
	return h
}

// foodQuickReplies 食物相關的快速按鈕.
func foodQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.CameraItem("拍下一餐"),
		models.CameraRollItem("從相簿選"),
		models.MessageItem("今日飲食", keywordTodayMeals),
		models.MessageItem("刪除上一筆", keywordDeleteMeal),
	}
}

// handleFoodKeyword 處理食物相關關鍵字；回傳 true 表示已處理.
func (h *WebhookHandler) handleFoodKeyword(ctx context.Context, userID, text, replyToken string) bool {
	switch {
	case text == keywordSnapFood:
		_ = h.line.ReplyText(ctx, replyToken, "拍一張餐點照片傳給我，我會估算熱量與營養素 🍽", foodQuickReplies()[:2]...)
	case todayMealAliases[text]:
		h.replyToday(ctx, userID, replyToken)
	case text == keywordDeleteMeal:
		h.deleteLatestMeal(ctx, userID, replyToken)
	default:
		return false
	}
	return true
}

// handleImageMessage 分析使用者傳來的照片.
func (h *WebhookHandler) handleImageMessage(ctx context.Context, event *models.Event) {
	userID, replyToken := event.Source.UserID, event.ReplyToken
	if h.nutrition == nil {
		_ = h.line.ReplyText(ctx, replyToken, "照片分析功能目前未啟用", weatherQuickReplies()...)
		return
	}
	if p := event.Message.ContentProvider.Type; p != "" && p != "line" {
		_ = h.line.ReplyText(ctx, replyToken, "請直接用 LINE 拍照或從相簿傳送照片", foodQuickReplies()[:2]...)
		return
	}
	if !h.withinDailyLimit(ctx, "photo", userID, photoDailyLimit) {
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("今天的照片分析已達 %d 張上限，明天再來喔！", photoDailyLimit))
		return
	}
	if err := h.line.StartLoading(ctx, userID, loadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}

	data, mime, err := h.line.DownloadImage(ctx, event.Message.ID)
	if err != nil {
		slog.Error("download image", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "照片讀取失敗，請再傳一次", foodQuickReplies()[:2]...)
		return
	}

	target, pending := h.photoTarget(ctx, userID)
	hint := ""
	switch {
	case target != nil:
		hint = target.Note
	case pending != nil:
		hint = pending.Text
	}
	actx, cancel := context.WithTimeout(ctx, imageAnalysisTimeout)
	analysis, err := h.nutrition.AnalyzeImage(actx, ai.Image{MIME: mime, Data: data}, hint)
	cancel()
	if err != nil {
		slog.Warn("analyze image", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "目前無法分析照片，請稍後再試", foodQuickReplies()[:2]...)
		return
	}

	switch {
	case analysis.Kind == nutrition.KindFood && target != nil:
		h.mergePhoto(ctx, userID, replyToken, target, analysis)
	case analysis.Kind == nutrition.KindFood:
		h.recordPhotoMeal(ctx, userID, replyToken, analysis, pending)
	case analysis.Kind == nutrition.KindExercise:
		h.recordScreenshot(ctx, userID, replyToken, analysis.Exercise)
	default:
		_ = h.line.ReplyText(ctx, replyToken, "這張看起來不是食物照片，拍下餐點再傳給我吧！", foodQuickReplies()[:2]...)
	}
}

// photoTarget 照片要併到哪一餐：追問中（只說了「便當」）的描述，或 10 分鐘內打字記錄的那一餐.
func (h *WebhookHandler) photoTarget(ctx context.Context, userID string) (*meal.Meal, *conversation.PendingMeal) {
	if state, err := h.convManager.Get(ctx, userID); err == nil && state != nil &&
		state.Type == conversation.TypeMealClarify && state.Meal != nil {
		h.clearState(ctx, userID)
		return nil, state.Meal
	}
	recent, ok := h.recent.Get(ctx, userID)
	if !ok || h.mealRepo == nil || recent.Source != meal.SourceText || h.recent.Now().Sub(recent.At) > photoMergeWindow {
		return nil, nil
	}
	m, err := h.mealRepo.FindByID(ctx, userID, recent.ID)
	if err != nil {
		slog.Warn("find meal to merge photo", "error", err)
		return nil, nil
	}
	return m, nil
}

// recordPhotoMeal 照片記成新的一餐；pending 是追問中的打字描述（可為 nil）.
func (h *WebhookHandler) recordPhotoMeal(
	ctx context.Context, userID, replyToken string, a *nutrition.ImageAnalysis, pending *conversation.PendingMeal,
) {
	now := time.Now()
	mealType, offset, source, note := nutrition.MealType(""), 0, meal.SourcePhoto, ""
	if pending != nil {
		mealType, offset, source, note = nutrition.MealType(pending.MealType), pending.DayOffset, meal.SourcePhotoText, pending.Text
	}
	eatenAt := nutrition.MealTime(now, mealType, offset)
	if !mealType.Valid() {
		mealType = nutrition.MealTypeAt(eatenAt)
	}
	m := &meal.Meal{
		UserID: userID, EatenAt: eatenAt, Items: a.Foods, Total: a.Total, Advice: a.Advice,
		Source: source, MealType: string(mealType), Note: note, Place: a.Place, CreateTime: now,
	}
	h.saveMeal(ctx, userID, replyToken, m, "🍱 已記錄"+dayPrefix(eatenAt, now)+mealType.Label()+nutrition.Disclaimer)
}

// mergePhoto 用照片重新估算剛打字記錄的那一餐（不新增一筆）. 使用者自己給的店家優先於照片辨識.
// target 是分析前讀出的；分析要好幾秒，寫回時以最新的資料為準（例如期間使用者記了店家）.
func (h *WebhookHandler) mergePhoto(ctx context.Context, userID, replyToken string, target *meal.Meal, a *nutrition.ImageAnalysis) {
	m := h.modifyMeal(ctx, userID, replyToken, target.ID, func(m *meal.Meal) {
		m.Items, m.Advice, m.Source = slices.Clone(a.Foods), a.Advice, meal.SourcePhotoText
		m.Rebase()
		if a.Place != nil && (m.Place == nil || m.Place.Source != meal.PlaceFromUser) {
			m.Place = a.Place
		}
	})
	if m == nil {
		return
	}
	h.recent.Remember(ctx, userID, m.ID, m.Source)
	header := "📷 已用照片更新剛剛的" + nutrition.MealType(m.MealType).Label() + nutrition.Disclaimer
	_ = h.line.ReplyText(ctx, replyToken, nutrition.FormatRecorded(m, header)+h.todayTotalLine(ctx, userID), mealActions(m)...)
}

func (h *WebhookHandler) todayMeals(ctx context.Context, userID string) ([]meal.Meal, error) {
	from, to := nutrition.DayRange(time.Now())
	return h.mealRepo.FindByUserBetween(ctx, userID, from, to)
}

func (h *WebhookHandler) replyToday(ctx context.Context, userID, replyToken string) {
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, "飲食紀錄功能目前未啟用")
		return
	}
	meals, err := h.todayMeals(ctx, userID)
	if err != nil {
		slog.Error("find today meals", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "讀取飲食紀錄失敗，請稍後再試")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, nutrition.FormatDay(meals), foodQuickReplies()[:3]...)
}

func (h *WebhookHandler) deleteLatestMeal(ctx context.Context, userID, replyToken string) {
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, "飲食紀錄功能目前未啟用")
		return
	}
	deleted, err := h.mealRepo.DeleteLatest(ctx, userID, time.Now().Add(-deleteWindow))
	switch {
	case err != nil:
		slog.Error("delete latest meal", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "刪除失敗，請稍後再試")
	case deleted == nil:
		_ = h.line.ReplyText(ctx, replyToken, "24 小時內沒有可以刪除的飲食紀錄")
	default:
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("已刪除 %s 的紀錄（%.0f 大卡）",
			deleted.EatenAt.In(nutrition.Taipei).Format("15:04"), deleted.Total.Calories), foodQuickReplies()[:3]...)
	}
}

// dailyNow 目前時間；測試可注入 h.clock.
func (h *WebhookHandler) dailyNow() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}

// withinDailyLimit 每人每天（台灣日期）的次數上限；Redis 沒設定或出錯（含 ctx 取消）時改用本地計數.
// [F24-32] 本地計數不淘汰，Redis 壞掉也擋得住.
func (h *WebhookHandler) withinDailyLimit(ctx context.Context, kind, userID string, limit int64) bool {
	key := kind + ":" + userID + ":" + h.dailyNow().In(nutrition.Taipei).Format("20060102")
	if h.limiter == nil {
		return h.daily.Incr(key, 26*time.Hour) <= limit
	}
	count, err := h.limiter.Incr(ctx, key).Result()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("daily limiter unavailable", "kind", kind, "error", err)
		}
		return h.daily.Incr(key, 26*time.Hour) <= limit
	}
	if count == 1 {
		h.limiter.Expire(ctx, key, 26*time.Hour)
	}
	return count <= limit
}
