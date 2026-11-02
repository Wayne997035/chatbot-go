package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/weather"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// 自由問答設定.
const (
	keywordAsk = "問 AI"
	// questionDailyLimit 每人每天的問答次數上限，保護付費額度.
	questionDailyLimit = 50
)

var askAliases = map[string]bool{keywordAsk: true, "問AI": true, "問ai": true, "問 ai": true}

// askPrefixes 以這些開頭的訊息直接當成問題.
var askPrefixes = []string{"問 ", "問：", "問:"}

// WithAssistant 啟用自由問答與地點記憶；兩者都可為 nil.
func (h *WebhookHandler) WithAssistant(a *assistant.Assistant, locations *weather.UserLocations) *WebhookHandler {
	h.assistant = a
	h.locations = locations
	return h
}

func askQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.MessageItem("今天還能吃多少？", "今天還能吃多少？"),
		models.MessageItem("晚餐吃什麼好？", "晚餐吃什麼比較好？"),
		models.MessageItem("今天適合跑步嗎？", "今天適合跑步嗎？"),
	}
}

// handleAskKeyword 處理「問 AI」按鈕與「問 xxx」；回傳 true 表示已處理.
func (h *WebhookHandler) handleAskKeyword(ctx context.Context, userID, text, replyToken string) bool {
	if askAliases[text] {
		if h.assistant == nil {
			_ = ReplyText(ctx, replyToken, "AI 問答目前未啟用", weatherQuickReplies()...)
			return true
		}
		_ = ReplyText(ctx, replyToken, "想問什麼都可以直接打字問我，例如飲食、運動或天氣 😊", askQuickReplies()...)
		return true
	}
	for _, p := range askPrefixes {
		if q, ok := strings.CutPrefix(text, p); ok && strings.TrimSpace(q) != "" {
			h.answer(ctx, userID, q, replyToken)
			return true
		}
	}
	return false
}

// answer 交給 AI 回答；沒有啟用時回覆查無地點（原本的行為）.
func (h *WebhookHandler) answer(ctx context.Context, userID, question, replyToken string) {
	if h.assistant == nil {
		_ = ReplyText(ctx, replyToken, "找不到「"+question+"」的天氣資料，請輸入區域名稱（如：大安區、天母），或直接傳送位置", weatherQuickReplies()...)
		return
	}
	if !h.allowQuestion(ctx, userID) {
		_ = ReplyText(ctx, replyToken, fmt.Sprintf("今天的 AI 問答已達 %d 次上限，明天再來喔！", questionDailyLimit))
		return
	}
	if err := StartLoading(ctx, userID, loadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}
	reply, err := h.assistant.Answer(ctx, userID, question)
	if err != nil {
		_ = ReplyText(ctx, replyToken, "目前無法回答，請稍後再試", weatherQuickReplies()...)
		return
	}
	if err := ReplyText(ctx, replyToken, reply, statsQuickReplies()[:3]...); err != nil {
		slog.Error("reply assistant answer", "error", err)
	}
}

// allowQuestion 每人每天的問答上限；Redis 不可用時不限制.
func (h *WebhookHandler) allowQuestion(ctx context.Context, userID string) bool {
	if h.limiter == nil {
		return true
	}
	key := "qa:count:" + userID + ":" + time.Now().In(nutrition.Taipei).Format("20060102")
	count, err := h.limiter.Incr(ctx, key).Result()
	if err != nil {
		slog.Warn("question limiter unavailable", "error", err)
		return true
	}
	if count == 1 {
		h.limiter.Expire(ctx, key, 26*time.Hour)
	}
	return count <= questionDailyLimit
}
