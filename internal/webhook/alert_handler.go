package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// isValidRegion 檢查是否為有效的縣市名稱.
func isValidRegion(name string) bool {
	for _, r := range taiwanRegions {
		if r == name {
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
	// [F242-06] 刪除資料之前送出的訂閱（含 LINE 重送的舊事件）不寫回.
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
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

	upsertErr := h.alertSubRepo.Upsert(ctx, sub)
	// [F242-06][F291-06] 寫後再查（回錯也查，可能已落地）：刪除流程先 mark 再 DeleteByUser，檢查與寫入之間完成的刪除這裡一定看得到標記；
	// 剛寫的這筆刪掉（ID 是 userID_type，只刪這一筆）；補刪失敗回 eraseFailedMessage，不謊稱已刪除.
	if h.guardAfterWrite(ctx, userID, replyToken, func(cctx context.Context) error {
		return h.alertSubRepo.Delete(cctx, sub.ID)
	}) {
		return
	}
	if upsertErr != nil {
		slog.Error("subscribe earthquake tsunami", "error", redact.Error(upsertErr))
		_ = h.line.ReplyText(ctx, replyToken, "訂閱失敗，請稍後再試")
		return
	}

	// [F24-98] 說清楚會收到什麼：資料是震後發布的報告，不是即時預警.
	_ = h.line.ReplyText(ctx, replyToken, "已開啟地震及海嘯警告通知！\n"+
		"會收到氣象署發布的全台顯著有感地震報告與海嘯資訊，約每 5 分鐘檢查一次，只通知 3 小時內發生的。\n"+
		"輸入「關閉地震海嘯警告」可以關閉。")
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

	err := h.convManager.Set(ctx, userID, state)
	// [F291-03] 選到一半的地區狀態：刪除後落地要清掉，不送縣市卡片.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if err != nil {
		slog.Error("set conversation state", "error", redact.Error(err))
		// Redis 不可用時，回覆提示但無法進行多輪對話
		_ = h.line.ReplyText(ctx, replyToken, "抱歉，目前無法啟動地區選擇，請稍後再試")
		return
	}

	// [F24-98] 卡片選縣市；altText 是看不到卡片時（通知預覽、舊版 LINE）顯示的文字.
	_ = h.line.ReplyFlex(ctx, replyToken, regionListMessage(), countyPickerBubble())
}

func (h *WebhookHandler) handleRegionSelection(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) {
	text = strings.TrimSpace(text)

	if isFinishKeyword(text) || text == keywordPickFinish {
		h.finishRegionSelection(ctx, userID, replyToken, state)
		return
	}

	// [F24-98] 每次點選都重設對話狀態，讓 5 分鐘從最後一次點選起算.
	var reply string
	switch kind, county, town := parsePick(text); kind {
	case pickCounty:
		if erased, undoErr := h.keepRegionSelection(ctx, userID, state); erased {
			h.replyErasedAfterWrite(ctx, replyToken, undoErr)
			return
		}
		_ = h.line.ReplyFlex(ctx, replyToken, "選擇"+county+"的鄉鎮市區", townshipPickerBubble(county))
		return
	case pickWholeCounty:
		reply = addWholeCounty(state, county)
	case pickTownship:
		reply = addTownship(state, county, town)
	default:
		reply = "找不到此區域，請點選卡片上的按鈕，或輸入縣市名稱"
	}
	if erased, undoErr := h.keepRegionSelection(ctx, userID, state); erased {
		h.replyErasedAfterWrite(ctx, replyToken, undoErr)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, reply, finishQuickReply())
}

// keepRegionSelection 存回選地區的對話狀態（也重設 5 分鐘到期）.
// [F291-03] 寫後再查：本函式沒有 replyToken，命中時回 erased=true 與補刪結果，由呼叫端用 replyErasedAfterWrite 回覆.
func (h *WebhookHandler) keepRegionSelection(ctx context.Context, userID string, state *conversation.State) (erased bool, undoErr error) {
	err := h.convManager.Set(ctx, userID, state)
	if erased, undoErr = h.recheckAfterWrite(ctx, userID, h.undoState(userID)); erased {
		return erased, undoErr
	}
	if err != nil {
		slog.Error("update conversation state", "error", redact.Error(err))
	}
	return false, nil
}

const pickMore = "還有嗎？可以再點上面的卡片，選好按「完成」"

// addWholeCounty 加入整個縣市；先移除這個縣市已選的鄉鎮（整縣已涵蓋）.
func addWholeCounty(state *conversation.State, county string) string {
	if slices.Contains(state.Regions, county) {
		return "「" + county + "」已在訂閱清單中，" + pickMore
	}
	state.Regions = slices.DeleteFunc(state.Regions, func(r string) bool { return strings.HasPrefix(r, county) })
	state.Regions = append(state.Regions, county)
	return "已加入「" + county + "」（整個縣市），" + pickMore
}

// addTownship 加入單一鄉鎮；已訂整個縣市就不用另外加.
func addTownship(state *conversation.State, county, town string) string {
	area := county + town
	switch {
	case slices.Contains(state.Regions, county):
		return "已訂閱整個" + county + "，不用另選" + town + "。" + pickMore
	case slices.Contains(state.Regions, area):
		return "「" + area + "」已在訂閱清單中，" + pickMore
	}
	state.Regions = append(state.Regions, area)
	return "已加入「" + area + "」，" + pickMore
}

// finishRegionSelection 「完成」：有選地區就存訂閱，沒選就取消.
func (h *WebhookHandler) finishRegionSelection(ctx context.Context, userID, replyToken string, state *conversation.State) {
	if len(state.Regions) == 0 {
		_ = h.convManager.Delete(ctx, userID)
		_ = h.line.ReplyText(ctx, replyToken, "未選擇任何地區，已取消天氣特報訂閱設定。")
		return
	}

	// [F242-06] 刪除資料之前送出的「完成」不寫回訂閱；選到一半的對話狀態一併清掉.
	if h.blockedByErasure(ctx, userID, replyToken) {
		_ = h.convManager.Delete(ctx, userID)
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

	upsertErr := h.alertSubRepo.Upsert(ctx, sub)
	// [F242-06][F291-06] 寫後再查（同地震海嘯）：補刪兩步都要做 —— 刪訂閱、清選地區的對話狀態；
	// 用會回傳錯誤的 Delete，任一步失敗都回 eraseFailedMessage.
	if h.guardAfterWrite(ctx, userID, replyToken, func(cctx context.Context) error {
		return joinUndo(cctx,
			func(c context.Context) error { return h.alertSubRepo.Delete(c, sub.ID) },
			func(c context.Context) error { return h.convManager.Delete(c, userID) },
		)
	}) {
		return
	}
	if upsertErr != nil {
		slog.Error("save weather warning sub", "error", redact.Error(upsertErr))
		_ = h.line.ReplyText(ctx, replyToken, "儲存訂閱失敗，請稍後再試")
		return
	}

	_ = h.convManager.Delete(ctx, userID)
	_ = h.line.ReplyText(ctx, replyToken,
		"已開啟天氣特報通知！\n訂閱地區："+strings.Join(state.Regions, "、")+
			"\n當這些地區有天氣特報時，將立即通知您。")
}

// taiwanRegions 台灣 22 縣市（清單本身在 region 套件，選單與特報比對共用同一份）.
var taiwanRegions = region.Counties
