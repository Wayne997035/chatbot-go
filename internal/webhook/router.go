package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// 這個檔案是訊息的路由表：一則訊息依序交給哪些處理步驟，順序就是優先權.
// 新增功能時在表裡找到它該在的位置，並在旁邊寫明為什麼排在那裡.

// messageHandlers 依訊息類型分派；沒有列出的類型回覆支援的類型.
var messageHandlers = map[string]func(h *WebhookHandler, ctx context.Context, event *models.Event){
	"text": func(h *WebhookHandler, ctx context.Context, event *models.Event) {
		h.handleTextMessage(ctx, event.Source.UserID, event.Message.Text, event.ReplyToken)
	},
	"location": func(h *WebhookHandler, ctx context.Context, event *models.Event) {
		// 正在問「在哪吃的」時，位置是這餐的店家；否則照常查天氣.
		if h.handleMealLocation(ctx, event.Source.UserID, &event.Message, event.ReplyToken) {
			return
		}
		h.handleLocationMessage(ctx, event.Source.UserID, event.Message.Address, event.ReplyToken)
	},
	"image": func(h *WebhookHandler, ctx context.Context, event *models.Event) {
		h.handleImageMessage(ctx, event)
	},
}

func (h *WebhookHandler) handleMessage(ctx context.Context, event *models.Event) {
	// [F24-06] 刪除確認只對緊接著的文字回覆有效；其他訊息類型（照片、貼圖、位置…）視為取消
	if event.Message.Type != "text" {
		h.clearDeleteConfirm(ctx, event.Source.UserID)
	}
	if handle, ok := messageHandlers[event.Message.Type]; ok {
		handle(h, ctx, event)
		return
	}
	_ = h.line.ReplyText(ctx, event.ReplyToken, "目前支援文字、位置與照片訊息，輸入「說明」看所有功能", weatherQuickReplies()...)
}

// 文字長度限制.
const (
	// maxInputRunes 一般訊息（問答、打字記餐）的上限.
	maxInputRunes = 500
	// maxPlaceQueryRunes 超過這個長度不可能是地名，不查天氣（Nominatim 查詢也有長度限制）.
	maxPlaceQueryRunes = 50
)

// textMessage 一則已整理過的文字訊息與使用者目前的對話狀態.
type textMessage struct {
	userID, text, replyToken string
	state                    *conversation.State // 沒有進行中的對話時為 nil
}

// textStep 文字訊息的一個處理步驟；回傳 true 表示已處理，後面的步驟不再執行.
type textStep struct {
	name string
	run  func(h *WebhookHandler, ctx context.Context, m *textMessage) bool
}

// textSteps 文字訊息依序嘗試的步驟.
var textSteps = []textStep{
	// 進行中的對話優先：選地點、選縣市、追問餐點、確認刪除…；使用者換話題時會清掉狀態、往下走.
	{"conversation", (*WebhookHandler).stepConversation},
	// 固定指令：說明、查天氣、今日統計、開關通知…
	{"keyword", (*WebhookHandler).stepKeyword},
	// 「午餐吃了牛肉麵」：要在查地名之前，否則店名（如「鼎泰豐」）會被當成地名去查天氣.
	{"meal", (*WebhookHandler).stepMeal},
	// 地名查天氣（精確 → 模糊 → geocoding）；太長的不可能是地名.
	{"weather", (*WebhookHandler).stepWeather},
	// 像在描述運動但規則看不懂的，交給 AI 解析（先請使用者確認才記錄）.
	{"workout", (*WebhookHandler).stepWorkout},
	// 都對不上 → 當成問題交給 AI 問答（未啟用時回覆查無地點）. 一定會處理，所以放最後.
	{"assistant", (*WebhookHandler).stepAssistant},
}

func (h *WebhookHandler) handleTextMessage(ctx context.Context, userID, text, replyToken string) {
	text = strings.ReplaceAll(strings.TrimSpace(text), "臺", "台")

	// 只有空白（例如只打了換行）
	if text == "" {
		_ = h.line.ReplyText(ctx, replyToken, "輸入「說明」看看我能做什麼 😊", weatherQuickReplies()...)
		return
	}
	// 超長輸入直接拒絕（避免拖慢服務與浪費 AI 額度）
	if utf8.RuneCountInString(text) > maxInputRunes {
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("訊息有點長，請精簡到 %d 字以內", maxInputRunes))
		return
	}

	state, err := h.convManager.Get(ctx, userID)
	if err != nil {
		slog.Error("get conversation state", "error", err)
	}
	m := &textMessage{userID: userID, text: text, replyToken: replyToken, state: state}
	for _, step := range textSteps {
		if step.run(h, ctx, m) {
			slog.Debug("text message handled", "step", step.name)
			return
		}
	}
}

// stepConversation 進行中的對話.
func (h *WebhookHandler) stepConversation(ctx context.Context, m *textMessage) bool {
	if m.state == nil {
		return false
	}
	switch m.state.Type {
	case conversation.TypeLocationClarify:
		h.handleLocationClarify(ctx, m.userID, m.text, m.replyToken, m.state)
		return true
	case conversation.TypeWeatherRegionSelect:
		h.handleRegionSelection(ctx, m.userID, m.text, m.replyToken, m.state)
		return true
	case conversation.TypeMealClarify, conversation.TypeMealPlace:
		// 回傳 false 表示使用者換了話題（狀態已清除），照一般流程處理這則訊息.
		return h.handleMealConversation(ctx, m.userID, m.text, m.replyToken, m.state)
	default:
		return h.handleFitnessConversation(ctx, m.userID, m.text, m.replyToken, m.state)
	}
}

func (h *WebhookHandler) stepKeyword(ctx context.Context, m *textMessage) bool {
	return h.handleKeyword(ctx, m.userID, m.text, m.replyToken)
}

func (h *WebhookHandler) stepMeal(ctx context.Context, m *textMessage) bool {
	return h.handleTextMeal(ctx, m.userID, m.text, m.replyToken, false)
}

func (h *WebhookHandler) stepWeather(ctx context.Context, m *textMessage) bool {
	return utf8.RuneCountInString(m.text) <= maxPlaceQueryRunes && h.replyWeatherQuery(ctx, m.userID, m.text, m.replyToken)
}

func (h *WebhookHandler) stepWorkout(ctx context.Context, m *textMessage) bool {
	return h.tryAIWorkout(ctx, m.userID, m.text, m.replyToken)
}

func (h *WebhookHandler) stepAssistant(ctx context.Context, m *textMessage) bool {
	h.answer(ctx, m.userID, m.text, m.replyToken)
	return true
}

// keywordStep 一組固定指令；回傳 true 表示是它的指令且已處理.
type keywordStep struct {
	name string
	run  func(h *WebhookHandler, ctx context.Context, userID, text, replyToken string) bool
}

// keywordSteps 固定指令依序比對. 各組指令互不重疊，順序只影響比對的先後.
var keywordSteps = []keywordStep{
	{"help", (*WebhookHandler).handleHelpKeyword},
	{"ask", (*WebhookHandler).handleAskKeyword},
	{"food", (*WebhookHandler).handleFoodKeyword},
	{"meal", (*WebhookHandler).handleMealCommand},
	{"fitness", (*WebhookHandler).handleFitnessKeyword},
	{"alert", (*WebhookHandler).handleAlertKeyword},
}

// handleKeyword 處理固定指令；回傳 true 表示已處理.
func (h *WebhookHandler) handleKeyword(ctx context.Context, userID, text, replyToken string) bool {
	for _, step := range keywordSteps {
		if step.run(h, ctx, userID, text, replyToken) {
			return true
		}
	}
	return false
}

// handleHelpKeyword 說明與「查天氣」按鈕.
func (h *WebhookHandler) handleHelpKeyword(ctx context.Context, _, text, replyToken string) bool {
	switch {
	case helpAliases[strings.ToLower(text)]:
		_ = h.line.ReplyText(ctx, replyToken, helpMessage, helpQuickReplies()...)
	case text == keywordWeather:
		_ = h.line.ReplyText(ctx, replyToken, "輸入地名（如：大安區、天母），或點下方按鈕傳送位置", weatherQuickReplies()...)
	default:
		return false
	}
	return true
}

// handleAlertKeyword 開關天氣特報與地震海嘯通知.
func (h *WebhookHandler) handleAlertKeyword(ctx context.Context, userID, text, replyToken string) bool {
	switch text {
	case keywordQuakeOn:
		h.handleEarthquakeTsunamiSubscribe(ctx, userID, replyToken)
	case keywordQuakeOff:
		h.handleUnsubscribe(ctx, userID, replyToken, alertsub.TypeEarthquakeTsunami, "已關閉地震及海嘯警告通知。")
	case keywordWeatherWarningOn:
		h.handleWeatherWarningSubscribe(ctx, userID, replyToken)
	case keywordWeatherWarningOff:
		h.handleUnsubscribe(ctx, userID, replyToken, alertsub.TypeWeatherWarning, "已關閉天氣特報通知。")
	default:
		return false
	}
	return true
}
