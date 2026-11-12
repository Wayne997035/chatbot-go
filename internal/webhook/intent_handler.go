package webhook

import (
	"chatbot-go/internal/intent"
	"chatbot-go/internal/models"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/subscription"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// [F30-61] 意圖步驟的上限與時間預算.
const (
	// intentDailyLimit 每人每天可以問意圖 AI 的次數（只計通過閘門的訊息）.
	intentDailyLimit = 60
	// intentTimeoutMax 意圖 AI 單次呼叫的逾時上限.
	intentTimeoutMax = 6 * time.Second
	// intentMinBudget 排完後少於這個時間就不問 AI.
	intentMinBudget = 2 * time.Second
	// replyMargin 留給送出回覆的時間.
	replyMargin = 4 * time.Second
	// assistantWorstCase 之後的問答最久要多久（對照 assistant.answerTimeout = 40 秒）.
	assistantWorstCase = 40 * time.Second
	// maxListedRegions 「我開了什麼」最多列出幾個地區，其餘寫「等 N 個地區」.
	maxListedRegions = 10
	// intentCloseLabel 關閉確認卡唯一的按鈕.
	intentCloseLabel = "確認關閉"
)

// IntentClassifier 判斷訊息意圖（*intent.Classifier 實作）；webhook 只依賴這一個方法，測試用假物件.
type IntentClassifier interface {
	Classify(ctx context.Context, text string) (intent.Result, error)
}

// WithIntent [F30-61] 啟用聊天意圖判斷；不呼叫（或傳 nil）時 stepIntent 一律不處理，等於 AI 停用.
func (h *WebhookHandler) WithIntent(c IntentClassifier) *WebhookHandler {
	h.intent = c
	return h
}

// intentTimeout [F30-61] 意圖 AI 的逾時 = min(6 秒, 剩餘時間 − 40 秒問答 − 4 秒回覆餘裕)；少於 2 秒就不問.
// 這樣意圖加問答加回覆不會超過事件的 55 秒上限（LINE reply token 約 1 分鐘內要用）.
func intentTimeout(remaining time.Duration) (time.Duration, bool) {
	t := min(intentTimeoutMax, remaining-assistantWorstCase-replyMargin)
	return t, t >= intentMinBudget
}

// stepIntent [F30-61] 沒被任何規則接住的訊息由 AI 判一次意圖，bot 用既有功能只對傳訊者執行.
// 回傳 false = 這一步不處理，訊息交給後面的問答（AI 停用、沒過閘門、額度用完、AI 失敗、意圖不明或信心不足、前置條件不成立）.
func (h *WebhookHandler) stepIntent(ctx context.Context, m *textMessage) bool {
	if h.intent == nil || !intent.PassesGate(m.text) {
		return false
	}
	timeout := intentTimeoutMax // ctx 沒有期限時視為剩餘時間無限大
	if deadline, ok := ctx.Deadline(); ok {
		var enough bool
		if timeout, enough = intentTimeout(time.Until(deadline)); !enough {
			return false
		}
	}
	// 閘門通過、真的要呼叫 AI 之前才計次；額度用完靜默交給問答.
	if !h.withinDailyLimit(ctx, "intent:count", m.userID, intentDailyLimit) {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := h.intent.Classify(cctx, m.text)
	if err != nil {
		// [F30-63] log 只記失敗類別，不記訊息、地名或 userID.
		reason := "invalid_output"
		if errors.Is(err, intent.ErrUnavailable) {
			reason = "unavailable"
		}
		slog.Warn("intent classify failed", "reason", reason)
		return false
	}
	slog.Info("intent classified", "intent", res.Intent, "confidence", res.Confidence, "provider", res.Provider, "model", res.Model)
	if res.Confidence != intent.ConfidenceHigh {
		return false
	}
	// [F30-63] 只有封閉列舉內的意圖；所有動作只用 m.userID，AI 輸出的字串不當回覆、userID、地區或文件 ID.
	switch res.Intent {
	case intent.ListSubscriptions:
		return h.intentList(ctx, m)
	case intent.WeatherWarningOn:
		h.handleWeatherWarningSubscribe(ctx, m.userID, m.replyToken)
		return true
	case intent.QuakeOn:
		h.handleEarthquakeTsunamiSubscribe(ctx, m.userID, m.replyToken)
		return true
	case intent.QuakeOff:
		return h.intentConfirmClose(ctx, m, alertsub.TypeEarthquakeTsunami)
	case intent.WeatherWarningOff:
		return h.intentConfirmClose(ctx, m, alertsub.TypeWeatherWarning)
	case intent.WeatherQuery:
		return h.intentWeatherQuery(ctx, m, res.Place)
	default:
		return false
	}
}

// subscriptionItem 傳訊者的某一種訂閱；沒有這筆或 Enabled=false 都算沒開. 讀取失敗回 err（呼叫端不得當成沒開）.
func (h *WebhookHandler) subscriptionItem(ctx context.Context, userID, alertType string) (item subscription.Item, on bool, err error) {
	items, err := h.subscriptions().List(ctx, userID)
	if err != nil {
		return subscription.Item{}, false, err
	}
	for _, it := range items {
		if it.AlertType == alertType {
			return it, it.Enabled, nil
		}
	}
	return subscription.Item{}, false, nil
}

// intentList [F30-62] 「我現在開了什麼通知」：固定樣板列出傳訊者的訂閱，只讀不寫.
func (h *WebhookHandler) intentList(ctx context.Context, m *textMessage) bool {
	items, err := h.subscriptions().List(ctx, m.userID)
	if err != nil {
		// 讀不到不能回「未開啟」，交給後面的問答.
		slog.Error("intent list subscriptions", "error", redact.Error(err))
		return false
	}
	weatherOn, quakeOn := false, false
	var regions []string
	for _, it := range items {
		switch {
		case it.AlertType == alertsub.TypeWeatherWarning && it.Enabled:
			weatherOn, regions = true, it.Regions
		case it.AlertType == alertsub.TypeEarthquakeTsunami && it.Enabled:
			quakeOn = true
		}
	}
	var b strings.Builder
	b.WriteString("目前的通知設定：\n天氣特報：")
	var buttons []models.QuickReplyItem
	if weatherOn {
		b.WriteString("已開啟（地區：" + listRegions(regions) + "）")
		buttons = append(buttons, models.MessageItem(truncateLabel("關閉天氣特報", maxQuickReplyLabelLen), keywordWeatherWarningOff))
	} else {
		b.WriteString("未開啟")
		buttons = append(buttons, models.MessageItem(truncateLabel("開啟天氣特報", maxQuickReplyLabelLen), keywordWeatherWarningOn))
	}
	b.WriteString("\n地震海嘯警告：")
	if quakeOn {
		b.WriteString("已開啟")
		buttons = append(buttons, models.MessageItem(truncateLabel("關閉地震海嘯警告", maxQuickReplyLabelLen), keywordQuakeOff))
	} else {
		b.WriteString("未開啟")
		buttons = append(buttons, models.MessageItem(truncateLabel("開啟地震海嘯警告", maxQuickReplyLabelLen), keywordQuakeOn))
	}
	_ = h.line.ReplyText(ctx, m.replyToken, b.String(), buttons...)
	return true
}

// listRegions 地區清單；太多時只列前幾個.
func listRegions(regions []string) string {
	if len(regions) <= maxListedRegions {
		return strings.Join(regions, "、")
	}
	return strings.Join(regions[:maxListedRegions], "、") + fmt.Sprintf(" 等 %d 個地區", len(regions))
}

// intentConfirmClose [F30-62] 關閉類：先讀狀態；沒開就說沒開，有開就出確認卡，不寫入.
// 確認卡只有一個快速回覆，文字就是固定的關閉指令，按下去由既有的關閉指令處理（不再叫 AI）.
func (h *WebhookHandler) intentConfirmClose(ctx context.Context, m *textMessage, alertType string) bool {
	item, on, err := h.subscriptionItem(ctx, m.userID, alertType)
	if err != nil {
		slog.Error("intent read subscription", "alertType", alertType, "error", redact.Error(err))
		return false
	}
	name, command, prompt := "地震海嘯警告", keywordQuakeOff, "要關閉地震海嘯警告通知嗎？"
	if alertType == alertsub.TypeWeatherWarning {
		name, command, prompt = "天氣特報", keywordWeatherWarningOff, "要關閉天氣特報通知嗎？"
		if on {
			prompt += "目前訂閱的地區：" + listRegions(item.Regions) + "，關閉後這些地區都會取消。"
		}
	}
	if !on {
		_ = h.line.ReplyText(ctx, m.replyToken, "目前沒有開啟"+name)
		return true
	}
	_ = h.line.ReplyText(ctx, m.replyToken, prompt,
		models.MessageItem(truncateLabel(intentCloseLabel, maxQuickReplyLabelLen), command))
	return true
}

// intentWeatherQuery [F30-62] weather_query：地名必須是傳訊者原文的子字串、2~50 字；走精確 → 模糊 → 地名服務.
// 查不到回 false（交給問答），這一步不回「找不到」.
func (h *WebhookHandler) intentWeatherQuery(ctx context.Context, m *textMessage, place string) bool {
	n := utf8.RuneCountInString(place)
	if n < minPlaceQueryRunes || n > maxPlaceQueryRunes || !strings.Contains(m.text, place) {
		return false
	}
	// [F30-66] 只有 AI 抽出（且是原文子字串）的地名才送地名服務.
	return h.replyWeatherQuery(ctx, m.userID, place, m.replyToken, true)
}
