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
	"unicode/utf8"
)

// 自由問答設定.
const (
	keywordAsk = "問 AI"
	// questionDailyLimit 每人每天的問答次數上限，保護付費額度.
	questionDailyLimit = 50
	// maxMealLogButtonRunes 超過這個長度不是單一食物名稱，不提供「記成一餐」.
	maxMealLogButtonRunes = 30
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
			_ = h.line.ReplyText(ctx, replyToken, "AI 問答目前未啟用", weatherQuickReplies()...)
			return true
		}
		_ = h.line.ReplyText(ctx, replyToken, "想問什麼都可以直接打字問我，例如飲食、運動或天氣 😊", askQuickReplies()...)
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
		_ = h.line.ReplyText(ctx, replyToken, "找不到「"+question+"」的天氣資料，請輸入區域名稱（如：大安區、天母），或直接傳送位置", weatherQuickReplies()...)
		return
	}
	if !h.withinDailyLimit(ctx, "qa:count", userID, questionDailyLimit) {
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("今天的 AI 問答已達 %d 次上限，明天再來喔！", questionDailyLimit))
		return
	}
	if err := h.line.StartLoading(ctx, userID, loadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}
	reply, err := h.assistant.Answer(ctx, userID, question)
	if err != nil {
		_ = h.line.ReplyText(ctx, replyToken, "目前無法回答，請稍後再試", weatherQuickReplies()...)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, reply.Text, h.answerQuickReplies(question, reply)...)
}

// answerQuickReplies 只說了一樣食物（「牛肉麵」）時多一個「記成一餐」，點了就直接記錄.
func (h *WebhookHandler) answerQuickReplies(question string, reply *assistant.Reply) []models.QuickReplyItem {
	items := statsQuickReplies()[:3]
	question = assistant.CleanQuestion(question)
	if reply.MentionsFood && h.nutrition != nil && h.mealRepo != nil && question != "" &&
		utf8.RuneCountInString(question) <= maxMealLogButtonRunes && !nutrition.LooksLikeQuestion(question) {
		return append([]models.QuickReplyItem{models.MessageItem("🍽 記成一餐", mealLogText(question))}, items[:2]...)
	}
	return items
}
