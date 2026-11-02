package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
)

var todayMealAliases = map[string]bool{keywordTodayMeals: true, "今天吃什麼": true, "今天吃了什麼": true, "飲食紀錄": true}

// WithNutrition 啟用食物照片分析；analyzer 為 nil 表示 AI 未啟用. mealRepo、limiter 可為 nil.
func (h *WebhookHandler) WithNutrition(analyzer *nutrition.Analyzer, mealRepo meal.MealRepository, limiter *redis.Client) *WebhookHandler {
	h.nutrition = analyzer
	h.mealRepo = mealRepo
	h.limiter = limiter
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
		_ = ReplyText(ctx, replyToken, "拍一張餐點照片傳給我，我會估算熱量與營養素 🍽", foodQuickReplies()[:2]...)
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
		_ = ReplyText(ctx, replyToken, "照片分析功能目前未啟用", weatherQuickReplies()...)
		return
	}
	if p := event.Message.ContentProvider.Type; p != "" && p != "line" {
		_ = ReplyText(ctx, replyToken, "請直接用 LINE 拍照或從相簿傳送照片", foodQuickReplies()[:2]...)
		return
	}
	if !h.allowPhoto(ctx, userID) {
		_ = ReplyText(ctx, replyToken, fmt.Sprintf("今天的照片分析已達 %d 張上限，明天再來喔！", photoDailyLimit))
		return
	}
	if err := StartLoading(ctx, userID, loadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}

	data, mime, err := DownloadImage(ctx, event.Message.ID)
	if err != nil {
		slog.Error("download image", "error", err)
		_ = ReplyText(ctx, replyToken, "照片讀取失敗，請再傳一次", foodQuickReplies()[:2]...)
		return
	}

	actx, cancel := context.WithTimeout(ctx, imageAnalysisTimeout)
	analysis, err := h.nutrition.AnalyzeImage(actx, ai.Image{MIME: mime, Data: data})
	cancel()
	if err != nil {
		slog.Warn("analyze image", "error", err)
		_ = ReplyText(ctx, replyToken, "目前無法分析照片，請稍後再試", foodQuickReplies()[:2]...)
		return
	}

	switch analysis.Kind {
	case nutrition.KindFood:
		h.recordMeal(ctx, userID, replyToken, analysis)
	case nutrition.KindExercise:
		h.recordScreenshot(ctx, userID, replyToken, analysis.Exercise)
	default:
		_ = ReplyText(ctx, replyToken, "這張看起來不是食物照片，拍下餐點再傳給我吧！", foodQuickReplies()[:2]...)
	}
}

// recordMeal 儲存這餐並回覆分析與今日累計.
func (h *WebhookHandler) recordMeal(ctx context.Context, userID, replyToken string, analysis *nutrition.ImageAnalysis) {
	reply := nutrition.FormatMeal(analysis)
	if h.mealRepo != nil {
		now := time.Now()
		m := &meal.Meal{UserID: userID, EatenAt: now, Items: analysis.Foods, Total: analysis.Total, Advice: analysis.Advice, CreateTime: now}
		if err := h.mealRepo.Insert(ctx, m); err != nil {
			slog.Error("save meal", "error", err)
			reply += "\n（這筆暫時無法儲存）"
		} else if meals, err := h.todayMeals(ctx, userID); err == nil {
			total := meal.Sum(meals)
			reply += fmt.Sprintf("\n今日累計：%.0f 大卡（%d 餐）", total.Calories, len(meals))
		}
	}
	if err := ReplyText(ctx, replyToken, reply, foodQuickReplies()...); err != nil {
		slog.Error("reply meal analysis", "error", err)
	}
}

func (h *WebhookHandler) todayMeals(ctx context.Context, userID string) ([]meal.Meal, error) {
	from, to := nutrition.DayRange(time.Now())
	return h.mealRepo.FindByUserBetween(ctx, userID, from, to)
}

func (h *WebhookHandler) replyToday(ctx context.Context, userID, replyToken string) {
	if h.mealRepo == nil {
		_ = ReplyText(ctx, replyToken, "飲食紀錄功能目前未啟用")
		return
	}
	meals, err := h.todayMeals(ctx, userID)
	if err != nil {
		slog.Error("find today meals", "error", err)
		_ = ReplyText(ctx, replyToken, "讀取飲食紀錄失敗，請稍後再試")
		return
	}
	_ = ReplyText(ctx, replyToken, nutrition.FormatDay(meals), foodQuickReplies()[:3]...)
}

func (h *WebhookHandler) deleteLatestMeal(ctx context.Context, userID, replyToken string) {
	if h.mealRepo == nil {
		_ = ReplyText(ctx, replyToken, "飲食紀錄功能目前未啟用")
		return
	}
	deleted, err := h.mealRepo.DeleteLatest(ctx, userID, time.Now().Add(-deleteWindow))
	switch {
	case err != nil:
		slog.Error("delete latest meal", "error", err)
		_ = ReplyText(ctx, replyToken, "刪除失敗，請稍後再試")
	case deleted == nil:
		_ = ReplyText(ctx, replyToken, "24 小時內沒有可以刪除的飲食紀錄")
	default:
		_ = ReplyText(ctx, replyToken, fmt.Sprintf("已刪除 %s 的紀錄（%.0f 大卡）",
			deleted.EatenAt.In(nutrition.Taipei).Format("15:04"), deleted.Total.Calories), foodQuickReplies()[:3]...)
	}
}

// allowPhoto 每人每天的照片分析上限；Redis 不可用時不限制.
func (h *WebhookHandler) allowPhoto(ctx context.Context, userID string) bool {
	if h.limiter == nil {
		return true
	}
	key := "photo:" + userID + ":" + time.Now().In(nutrition.Taipei).Format("20060102")
	count, err := h.limiter.Incr(ctx, key).Result()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("photo limiter unavailable", "error", err)
		}
		return true
	}
	if count == 1 {
		h.limiter.Expire(ctx, key, 26*time.Hour)
	}
	return count <= photoDailyLimit
}
