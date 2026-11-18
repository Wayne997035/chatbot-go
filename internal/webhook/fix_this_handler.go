package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"chatbot-go/internal/redact"
	"context"
	"log/slog"
)

// 「修正這筆」按鈕：按下後短時間內客人的下一句直接進修改規劃器（B 武裝式）.
const (
	// keywordFixThis [F34-62] 按鈕送出的文字（message action，label 與 text 相同）.
	keywordFixThis = "修正這筆"

	// fixThisPrompt [F34-63] 按下後的固定提示；不叫 AI、不寫暫存、不寫紀錄.
	fixThisPrompt = "要怎麼修正？直接用一句話告訴我，例如：\n" +
		"・「把午餐的麵包改成 210 大卡」\n" +
		"・「拿掉滷蛋」\n" +
		"・「這餐改成晚餐」\n" +
		"・「跑步改成 30 分鐘」\n" +
		"我會先列出要改的內容，你按確認才會更新。"

	// fixThisNoRecordsMessage [F34-63] 近 7 天沒有任何可改的紀錄.
	fixThisNoRecordsMessage = "目前沒有可以修正的紀錄（只能改近 7 天內的），先傳吃了什麼或做了什麼運動給我記錄"
)

// fixThisButtons [F34-62] 「修正這筆」按鈕；聊天改紀錄停用時不放（客人點了也沒東西可接）.
func (h *WebhookHandler) fixThisButtons() []models.QuickReplyItem {
	if !h.recordEditEnabled() {
		return nil
	}
	return []models.QuickReplyItem{models.MessageItem(keywordFixThis, keywordFixThis)}
}

// handleFixThisKeyword [F34-63] 整句全等「修正這筆」才處理；停用時回 false（照舊往下）.
// 武裝只存 State{Type: TypeRecordFix}：不存紀錄 ID、不存文字、不存目標（目標由規劃器找）.
func (h *WebhookHandler) handleFixThisKeyword(ctx context.Context, userID, text, replyToken string) bool {
	if text != keywordFixThis || !h.recordEditEnabled() {
		return false
	}
	// [F34-65] 刪除之後才處理到的武裝不寫（同 askMealClarify）.
	if h.blockedByErasure(ctx, userID, replyToken) {
		return true
	}
	if !h.hasEditableRecords(ctx, userID) {
		_ = h.line.ReplyText(ctx, replyToken, fixThisNoRecordsMessage)
		return true
	}
	// 沒有對話管理：降級成只回提示，不假裝已武裝（Set 對不可用的 Manager 是空操作）.
	if !h.convManager.Available() {
		h.replyFixThisPrompt(ctx, replyToken)
		return true
	}
	setErr := h.convManager.Set(ctx, userID, &conversation.State{Type: conversation.TypeRecordFix})
	// [F34-65] 寫後再查：檢查與寫入之間完成的刪除，清掉剛寫的狀態並回已刪除，不得再回提示.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return true
	}
	if setErr != nil {
		slog.Error("arm record fix", "error", redact.Error(setErr)) // 降級成提示式；不記使用者文字
	}
	h.replyFixThisPrompt(ctx, replyToken)
	return true
}

func (h *WebhookHandler) replyFixThisPrompt(ctx context.Context, replyToken string) {
	_ = h.line.ReplyText(ctx, replyToken, fixThisPrompt, models.MessageItem("今日飲食", keywordTodayMeals))
}

// fixPassThrough [F34-64] 武裝期間直通的句子 = 選單指令（含「修正這筆」本身，再按一次重新武裝）＋全等的「確認」「取消」.
func fixPassThrough(text string) bool {
	return isMenuCommand(text) || text == typedConfirm || text == typedCancel
}

// handleRecordFixConversation [F34-64] 武裝後的下一句：先清狀態（用一次就清）；直通清單回 false 交給後面的步驟；
// 其餘跳過 EditGate 直接進修改規劃器（照扣每日額度），規劃器沒出卡（回 false）時訊息照舊往下.
func (h *WebhookHandler) handleRecordFixConversation(ctx context.Context, m *textMessage) bool {
	h.clearState(ctx, m.userID)
	if fixPassThrough(m.text) {
		return false
	}
	return h.runRecordEdit(ctx, m)
}

// clearRecordFix [F34-64] 非文字訊息（照片、位置、貼圖…）打斷武裝；讀不到狀態時保守地清掉（寫法同 clearDeleteConfirm）.
func (h *WebhookHandler) clearRecordFix(ctx context.Context, userID string) {
	state, err := h.convManager.GetStrict(ctx, userID)
	if err != nil {
		slog.Warn("read conversation state for record fix failed, clearing it", "error", redact.Error(err))
		h.clearState(ctx, userID)
		return
	}
	if state != nil && state.Type == conversation.TypeRecordFix {
		h.clearState(ctx, userID)
	}
}
