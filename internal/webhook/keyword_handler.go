package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// taiwanRegions 台灣 22 縣市.
var taiwanRegions = []string{
	"台北市", "新北市", "桃園市", "台中市", "台南市", "高雄市",
	"基隆市", "新竹市", "嘉義市", "新竹縣", "苗栗縣", "彰化縣",
	"南投縣", "雲林縣", "嘉義縣", "屏東縣", "宜蘭縣", "花蓮縣",
	"台東縣", "澎湖縣", "金門縣", "連江縣",
}

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

// finishKeywords 結束對話的關鍵字.
var finishKeywords = []string{"完成", "好了", "不用了", "結束"}

func isFinishKeyword(text string) bool {
	for _, kw := range finishKeywords {
		if text == kw {
			return true
		}
	}
	return false
}

// handleConversation 處理進行中的對話狀態；回傳 true 表示已處理.
func (h *WebhookHandler) handleConversation(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) bool {
	if state == nil {
		return false
	}

	if state.Type == conversation.TypeWeatherRegionSelect {
		h.handleRegionSelection(ctx, userID, text, replyToken, state)
		return true
	}
	if state.Type == conversation.TypeMealClarify || state.Type == conversation.TypeMealPlace {
		// 回傳 false 表示使用者換了話題（狀態已清除），照一般流程處理這則訊息.
		return h.handleMealConversation(ctx, userID, text, replyToken, state)
	}
	return h.handleFitnessConversation(ctx, userID, text, replyToken, state)
}

// 關鍵字.
const (
	keywordHelp             = "說明"
	keywordWeatherWarningOn = "開啟天氣特報"
	keywordQuakeOn          = "開啟地震海嘯警告"
	keywordWeather          = "查天氣"
)

// helpAliases 都會顯示使用說明.
var helpAliases = map[string]bool{keywordHelp: true, "使用說明": true, "功能": true, "選單": true, "help": true, "?": true, "？": true}

// helpMessage 使用說明.
const helpMessage = "我可以幫你：\n" +
	"🌤 查天氣：輸入地名（如：大安區、天母、日月潭），或傳送位置\n" +
	"🍱 記飲食：拍照或直接打字（如：午餐吃了牛肉麵），估算熱量與營養素；「今日飲食」看今天吃了什麼\n" +
	"🏃 記運動：輸入「跑步 5公里 30分鐘」或傳運動 App 截圖，自動計算消耗熱量\n" +
	"📊 統計：「今日統計」「本週統計」看熱量收支；「體重 70 身高 170 30歲 男」設定身體資料\n" +
	"💬 問 AI：飲食、運動、天氣都可以直接打字問我\n" +
	"⚠️ 天氣特報：輸入「開啟天氣特報」選擇縣市，有特報時通知你\n" +
	"🌏 地震海嘯：輸入「開啟地震海嘯警告」\n" +
	"關閉通知：「關閉天氣特報」、「關閉地震海嘯警告」\n" +
	"刪除所有個人資料：「刪除我的資料」"

// handleKeyword 處理關鍵字指令；回傳 true 表示已處理.
func (h *WebhookHandler) handleKeyword(ctx context.Context, userID, text, replyToken string) bool {
	if helpAliases[strings.ToLower(text)] {
		_ = ReplyText(ctx, replyToken, helpMessage,
			models.LocationItem("傳送位置查天氣"),
			models.CameraItem("拍食物"),
			models.MessageItem("記運動", keywordLogWorkout),
			models.MessageItem("今日統計", keywordToday),
			models.MessageItem("開啟天氣特報", keywordWeatherWarningOn),
			models.MessageItem("開啟地震警告", keywordQuakeOn),
		)
		return true
	}
	if text == keywordWeather {
		_ = ReplyText(ctx, replyToken, "輸入地名（如：大安區、天母），或點下方按鈕傳送位置", weatherQuickReplies()...)
		return true
	}
	if h.handleAskKeyword(ctx, userID, text, replyToken) || h.handleFoodKeyword(ctx, userID, text, replyToken) ||
		h.handleMealCommand(ctx, userID, text, replyToken) || h.handleFitnessKeyword(ctx, userID, text, replyToken) {
		return true
	}
	switch text {
	case keywordQuakeOn:
		h.handleEarthquakeTsunamiSubscribe(ctx, userID, replyToken)
		return true
	case "關閉地震海嘯警告":
		h.handleUnsubscribe(ctx, userID, replyToken, alertsub.TypeEarthquakeTsunami, "已關閉地震及海嘯警告通知。")
		return true
	case keywordWeatherWarningOn:
		h.handleWeatherWarningSubscribe(ctx, userID, replyToken)
		return true
	case "關閉天氣特報":
		h.handleUnsubscribe(ctx, userID, replyToken, alertsub.TypeWeatherWarning, "已關閉天氣特報通知。")
		return true
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
		slog.Error("subscribe earthquake tsunami", "userID", userID, "error", err)
		_ = ReplyText(ctx, replyToken, "訂閱失敗，請稍後再試")
		return
	}

	_ = ReplyText(ctx, replyToken, "已開啟地震及海嘯警告通知！\n當有地震或海嘯警報時，將立即通知您。")
}

// handleUnsubscribe 取消訂閱共用邏輯.
func (h *WebhookHandler) handleUnsubscribe(
	ctx context.Context, userID, replyToken, alertType, successMsg string,
) {
	id := fmt.Sprintf("%s_%s", userID, alertType)
	if err := h.alertSubRepo.Delete(ctx, id); err != nil {
		slog.Error("unsubscribe alert", "alertType", alertType, "userID", userID, "error", err)
		_ = ReplyText(ctx, replyToken, "取消訂閱失敗，請稍後再試")
		return
	}

	_ = ReplyText(ctx, replyToken, successMsg)
}

func (h *WebhookHandler) handleWeatherWarningSubscribe(ctx context.Context, userID, replyToken string) {
	// 啟動地區選擇對話
	state := &conversation.State{
		Type:    conversation.TypeWeatherRegionSelect,
		Regions: []string{},
	}

	if err := h.convManager.Set(ctx, userID, state); err != nil {
		slog.Error("set conversation state", "userID", userID, "error", err)
		// Redis 不可用時，回覆提示但無法進行多輪對話
		_ = ReplyText(ctx, replyToken, "抱歉，目前無法啟動地區選擇，請稍後再試")
		return
	}

	_ = ReplyText(ctx, replyToken, regionListMessage())
}

func (h *WebhookHandler) handleRegionSelection(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) {
	text = strings.TrimSpace(text)

	if isFinishKeyword(text) {
		if len(state.Regions) == 0 {
			_ = h.convManager.Delete(ctx, userID)
			_ = ReplyText(ctx, replyToken, "未選擇任何地區，已取消天氣特報訂閱設定。")
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
			slog.Error("save weather warning sub", "userID", userID, "error", err)
			_ = ReplyText(ctx, replyToken, "儲存訂閱失敗，請稍後再試")
			return
		}

		_ = h.convManager.Delete(ctx, userID)
		_ = ReplyText(ctx, replyToken,
			"已開啟天氣特報通知！\n訂閱地區："+strings.Join(state.Regions, "、")+
				"\n當這些地區有天氣特報時，將立即通知您。")
		return
	}

	if !isValidRegion(text) {
		_ = ReplyText(ctx, replyToken, "找不到此區域，請輸入正確的縣市名稱")
		return
	}

	// 檢查是否已加入
	for _, r := range state.Regions {
		if r == text {
			_ = ReplyText(ctx, replyToken, "「"+text+"」已在訂閱清單中，還有嗎？輸入「完成」結束選擇")
			return
		}
	}

	state.Regions = append(state.Regions, text)
	if err := h.convManager.Set(ctx, userID, state); err != nil {
		slog.Error("update conversation state", "userID", userID, "error", err)
	}

	_ = ReplyText(ctx, replyToken, "已加入「"+text+"」，還有嗎？輸入「完成」結束選擇")
}

// handleLocationClarify 處理地點澄清對話（多候選地點時）.
func (h *WebhookHandler) handleLocationClarify(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) {
	candidates := state.Candidates
	if len(candidates) == 0 {
		_ = h.convManager.Delete(ctx, userID)
		_ = ReplyText(ctx, replyToken, "找不到候選地點，請重新輸入地區名稱")
		return
	}

	selected := findClarifyCandidate(text, candidates)
	if selected == nil {
		_ = ReplyFlex(ctx, replyToken, "找到多個符合的地點，請選擇", buildCandidateFlexMessage(candidates))
		return
	}

	// 清除對話狀態
	if err := h.convManager.Delete(ctx, userID); err != nil {
		slog.Error("delete location clarify state", "userID", userID, "error", err)
	}

	// 查詢天氣
	forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, selected.City, selected.District)
	if err != nil {
		slog.Error("weather lookup after clarify", "city", selected.City, "district", selected.District, "error", err)
		_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return
	}

	if forecast == nil {
		_ = ReplyText(ctx, replyToken, "抱歉找不到該地點的天氣資料")
		return
	}

	note := ""
	if selected.Source == "geocode" {
		note = osmAttribution
	}
	h.replyForecast(ctx, userID, replyToken, forecast, note)
}

// findClarifyCandidate 從 candidates 中根據 user 輸入找到選擇的候選地點.
// 支援：數字索引（"1"、"2"...）或包含 district 名稱的文字.
func findClarifyCandidate(text string, candidates []conversation.CandidateLocation) *conversation.CandidateLocation {
	text = strings.TrimSpace(text)

	// 嘗試數字選擇
	if idx, err := strconv.Atoi(text); err == nil {
		if idx >= 1 && idx <= len(candidates) {
			c := candidates[idx-1]
			return &c
		}
		return nil
	}

	// 嘗試地名比對（包含 district）
	for i, c := range candidates {
		if strings.Contains(text, c.District) || strings.Contains(c.District, text) {
			result := candidates[i]
			return &result
		}
	}

	return nil
}
