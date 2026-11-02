package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// isValidRegion 檢查是否為有效的縣市名稱.
func isValidRegion(region string) bool {
	for _, r := range taiwanRegions {
		if r == region {
			return true
		}
	}
	return false
}

// regionListMessage 產生縣市列表訊息.
func regionListMessage() string {
	return "請輸入您想訂閱的縣市名稱（可多個），輸入「完成」結束選擇：\n" +
		strings.Join(taiwanRegions, "、")
}

func isFinishKeyword(text string) bool {
	for _, kw := range finishKeywords {
		if text == kw {
			return true
		}
	}
	return false
}

func (h *WebhookHandler) handleEarthquakeTsunamiSubscribe(ctx context.Context, userID, replyToken string) {
	now := time.Now()
	sub := &alertsub.AlertSubscription{
		ID:         fmt.Sprintf("%s_%s", userID, alertsub.TypeEarthquakeTsunami),
		UserID:     userID,
		AlertType:  alertsub.TypeEarthquakeTsunami,
		Regions:    []string{},
		Enabled:    true,
		CreateTime: now,
		UpdateTime: now,
	}

	if err := h.alertSubRepo.Upsert(ctx, sub); err != nil {
		slog.Error("subscribe earthquake tsunami", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "訂閱失敗，請稍後再試")
		return
	}

	_ = h.line.ReplyText(ctx, replyToken, "已開啟地震及海嘯警告通知！\n當有地震或海嘯警報時，將立即通知您。")
}

// handleUnsubscribe 取消訂閱共用邏輯.
func (h *WebhookHandler) handleUnsubscribe(
	ctx context.Context, userID, replyToken, alertType, successMsg string,
) {
	id := fmt.Sprintf("%s_%s", userID, alertType)
	if err := h.alertSubRepo.Delete(ctx, id); err != nil {
		slog.Error("unsubscribe alert", "alertType", alertType, "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "取消訂閱失敗，請稍後再試")
		return
	}

	_ = h.line.ReplyText(ctx, replyToken, successMsg)
}

func (h *WebhookHandler) handleWeatherWarningSubscribe(ctx context.Context, userID, replyToken string) {
	// 啟動地區選擇對話
	state := &conversation.State{
		Type:    conversation.TypeWeatherRegionSelect,
		Regions: []string{},
	}

	if err := h.convManager.Set(ctx, userID, state); err != nil {
		slog.Error("set conversation state", "error", err)
		// Redis 不可用時，回覆提示但無法進行多輪對話
		_ = h.line.ReplyText(ctx, replyToken, "抱歉，目前無法啟動地區選擇，請稍後再試")
		return
	}

	_ = h.line.ReplyText(ctx, replyToken, regionListMessage())
}

func (h *WebhookHandler) handleRegionSelection(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) {
	text = strings.TrimSpace(text)

	if isFinishKeyword(text) {
		if len(state.Regions) == 0 {
			_ = h.convManager.Delete(ctx, userID)
			_ = h.line.ReplyText(ctx, replyToken, "未選擇任何地區，已取消天氣特報訂閱設定。")
			return
		}

		now := time.Now()
		sub := &alertsub.AlertSubscription{
			ID:         fmt.Sprintf("%s_%s", userID, alertsub.TypeWeatherWarning),
			UserID:     userID,
			AlertType:  alertsub.TypeWeatherWarning,
			Regions:    state.Regions,
			Enabled:    true,
			CreateTime: now,
			UpdateTime: now,
		}

		if err := h.alertSubRepo.Upsert(ctx, sub); err != nil {
			slog.Error("save weather warning sub", "error", err)
			_ = h.line.ReplyText(ctx, replyToken, "儲存訂閱失敗，請稍後再試")
			return
		}

		_ = h.convManager.Delete(ctx, userID)
		_ = h.line.ReplyText(ctx, replyToken,
			"已開啟天氣特報通知！\n訂閱地區："+strings.Join(state.Regions, "、")+
				"\n當這些地區有天氣特報時，將立即通知您。")
		return
	}

	if !isValidRegion(text) {
		_ = h.line.ReplyText(ctx, replyToken, "找不到此區域，請輸入正確的縣市名稱")
		return
	}

	// 檢查是否已加入
	for _, r := range state.Regions {
		if r == text {
			_ = h.line.ReplyText(ctx, replyToken, "「"+text+"」已在訂閱清單中，還有嗎？輸入「完成」結束選擇")
			return
		}
	}

	state.Regions = append(state.Regions, text)
	if err := h.convManager.Set(ctx, userID, state); err != nil {
		slog.Error("update conversation state", "error", err)
	}

	_ = h.line.ReplyText(ctx, replyToken, "已加入「"+text+"」，還有嗎？輸入「完成」結束選擇")
}

// taiwanRegions 台灣 22 縣市.
var taiwanRegions = []string{
	"台北市", "新北市", "桃園市", "台中市", "台南市", "高雄市",
	"基隆市", "新竹市", "嘉義市", "新竹縣", "苗栗縣", "彰化縣",
	"南投縣", "雲林縣", "嘉義縣", "屏東縣", "宜蘭縣", "花蓮縣",
	"台東縣", "澎湖縣", "金門縣", "連江縣",
}
