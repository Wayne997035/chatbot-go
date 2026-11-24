package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/intent"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
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
		h.clearRecordFix(ctx, event.Source.UserID) // [F34-64] 武裝只對緊接著的文字有效
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
	// minPlaceQueryRunes [F26-05] 一個字不查地名：Nominatim 模糊比對會把單字（「嗨」「好」）對到不相干的地點.
	minPlaceQueryRunes = 2
)

// textMessage 一則已整理過的文字訊息與使用者目前的對話狀態.
type textMessage struct {
	userID, text, replyToken string
	state                    *conversation.State // 沒有進行中的對話時為 nil
	// editTried [F33-22] 這則訊息已經叫過修改規劃器（無論結果）；同一則訊息不叫第二次.
	editTried bool
}

// textStep 文字訊息的一個處理步驟；回傳 true 表示已處理，後面的步驟不再執行.
type textStep struct {
	name string
	run  func(h *WebhookHandler, ctx context.Context, m *textMessage) bool
}

// textSteps 文字訊息依序嘗試的步驟.
var textSteps = []textStep{
	// [F293-05] 「確認刪除」只回應等待中的刪除確認；其他時候是舊按鈕或確認已過期：回過期提示、不動對話狀態，
	// 否則會被當成地名查天氣、交給 AI，或在等運動確認時記成一筆運動.
	{"staleDeleteConfirm", (*WebhookHandler).stepStaleDeleteConfirm},
	// 進行中的對話優先：選地點、選縣市、追問餐點、確認刪除…；使用者換話題時會清掉狀態、往下走.
	{"conversation", (*WebhookHandler).stepConversation},
	// [F34-17] 打字「確認」「取消」＝按確認卡的按鈕（只認全等；沒有待確認回固定句）. 排在 conversation 之後：進行中的對話（運動確認的「確認」）優先；
	// 排在 stepAssistant 之前：不會落到問答.
	{"recordEditConfirmText", (*WebhookHandler).stepRecordEditConfirmText},
	// [F33-22] 「跑步改成 30 分鐘」會被 keyword 步驟裡的規則式運動解析直接記成一筆新運動，所以這種句子的修改快判要排在 keyword 之前；
	// 規劃器回 None（或功能停用）時回 false，keyword 照舊把它記成運動，行為不變.
	{"recordEditWorkoutRule", (*WebhookHandler).stepRecordEditWorkoutRule},
	// 固定指令：說明、查天氣、今日統計、開關通知…
	{"keyword", (*WebhookHandler).stepKeyword},
	// [F33-22] 「把午餐改成牛肉麵」符合記餐、「跑步改成 30 分鐘」符合記運動，所以修改的快判一定要排在 meal 與 workout 之前，
	// 否則會被記成新的一筆. 沒命中關鍵字、功能沒啟用、規劃器回 None 時回 false，後面的步驟照舊.
	{"recordEdit", (*WebhookHandler).stepRecordEdit},
	// 「午餐吃了牛肉麵」：要在查地名之前，否則店名（如「鼎泰豐」）會被當成地名去查天氣.
	{"meal", (*WebhookHandler).stepMeal},
	// 地名查天氣（精確 → 模糊；原文不送 geocoding，見 stepIntent）；太長的不可能是地名.
	{"weather", (*WebhookHandler).stepWeather},
	// [F36-05] 像在描述運動但規則看不懂的：回固定教學句（不叫 AI、不落問答）.
	{"workout", (*WebhookHandler).stepWorkout},
	// [F30-61] 聊天意圖（一般說法開關通知、查地標天氣）：一定要在所有規則之後，才只會看到沒被任何規則或對話狀態接住的訊息；
	// 又要在自由問答之前，否則問答會先吃掉它. AI 停用、沒過閘門、AI 失敗或意圖不明時回 false，照舊交給問答.
	{"intent", (*WebhookHandler).stepIntent},
	// 都對不上 → 當成問題交給 AI 問答（未啟用時回覆查無地點）. 一定會處理，所以放最後.
	{"assistant", (*WebhookHandler).stepAssistant},
}

func (h *WebhookHandler) handleTextMessage(ctx context.Context, userID, text, replyToken string) {
	// [F36-01] 冪等：經 handleEventSafely 來的已有額度，直呼本函式的測試也受管.
	ctx = ai.EnsureBudget(ctx)
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

// stepStaleDeleteConfirm [F293-05] 沒有在等刪除確認時收到「確認刪除」：回過期提示；等待中的刪除確認交給 stepConversation.
func (h *WebhookHandler) stepStaleDeleteConfirm(ctx context.Context, m *textMessage) bool {
	if m.text != keywordConfirmDelete || (m.state != nil && m.state.Type == conversation.TypeDeleteDataConfirm) {
		return false
	}
	_ = h.line.ReplyText(ctx, m.replyToken, staleDeleteConfirmMessage)
	return true
}

// stepConversation 進行中的對話.
func (h *WebhookHandler) stepConversation(ctx context.Context, m *textMessage) bool {
	if m.state == nil {
		return false
	}
	switch m.state.Type {
	case conversation.TypeLocationClarify:
		// [F26-04] 回傳 false 表示使用者換了話題（狀態已清除），照一般流程處理這則訊息.
		return h.handleLocationClarify(ctx, m.userID, m.text, m.replyToken, m.state)
	case conversation.TypeWeatherRegionSelect:
		h.handleRegionSelection(ctx, m.userID, m.text, m.replyToken, m.state)
		return true
	case conversation.TypeMealClarify, conversation.TypeMealPlace:
		// 回傳 false 表示使用者換了話題（狀態已清除），照一般流程處理這則訊息.
		return h.handleMealConversation(ctx, m.userID, m.text, m.replyToken, m.state)
	case conversation.TypeRecordFix:
		// [F34-64] 回傳 false 表示直通（選單指令、打字確認／取消）或規劃器沒出卡；狀態已清，照一般流程處理.
		return h.handleRecordFixConversation(ctx, m)
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
	n := utf8.RuneCountInString(m.text)
	// [F30-66][F36-04] 打字原文只比對官方縣市鄉鎮（精確、模糊），不送地名服務；單打地標（日月潭）不再查天氣，要寫「地標＋天氣」才由 stepIntent 的意圖 AI 抽地名.
	return n >= minPlaceQueryRunes && n <= maxPlaceQueryRunes && h.replyWeatherQuery(ctx, m.userID, m.text, m.replyToken, false)
}

// stepWorkout [F36-05] 運動只用規則：走到這裡代表 keyword 步驟的 handleWorkoutText 沒接住.
// 含運動提示字、不是問句、不含意圖提示字 → 回固定教學句（不叫 AI、不落問答、不寫狀態或紀錄）；
// 問句與含意圖提示字的訊息（通知句、含時段字/吃/喝的句子）照舊往下走意圖/問答（決策 fcfd7d54）.
func (h *WebhookHandler) stepWorkout(ctx context.Context, m *textMessage) bool {
	if h.workoutRepo == nil || !fitness.LooksLikeWorkout(m.text) || nutrition.LooksLikeQuestion(m.text) || intent.PassesGate(m.text) {
		return false
	}
	_ = h.line.ReplyText(ctx, m.replyToken, workoutGuideMessage, activityQuickReplies()...)
	return true
}

func (h *WebhookHandler) stepAssistant(ctx context.Context, m *textMessage) bool {
	h.answer(ctx, m.userID, m.text, m.replyToken, m.editTried) // [F34-04] 規劃器試過沒出卡 → 問答帶【系統備註】
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
	// [F34-60] 單打「記錄」：排在 food 與 meal 之前、不依賴任何 AI 元件，才不會落到意圖 AI 或問答被誤答「已登錄」.
	{"bareLog", (*WebhookHandler).handleBareLogKeyword},
	// [F34-63] 「修正這筆」：全等才處理、停用時往下；排在 meal 之前（含「修正」會被聊天改紀錄的快判吃掉而燒額度）.
	{"fixThis", (*WebhookHandler).handleFixThisKeyword},
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

// weatherPromptMessage [F36-04] 「查天氣」按鈕的回覆：單打地標不再查天氣，要寫「地標＋天氣」（決策 2894f439）.
const weatherPromptMessage = "輸入地名（如：大安區、天母；地標請加「天氣」，如：日月潭天氣），或點下方按鈕傳送位置"

// handleHelpKeyword 說明與「查天氣」按鈕.
func (h *WebhookHandler) handleHelpKeyword(ctx context.Context, _, text, replyToken string) bool {
	switch {
	case helpAliases[strings.ToLower(text)]:
		_ = h.line.ReplyText(ctx, replyToken, helpMessage, helpQuickReplies()...)
	case text == keywordWeather:
		_ = h.line.ReplyText(ctx, replyToken, weatherPromptMessage, weatherQuickReplies()...)
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
		// [F24-98] 選地區的對話已過期，使用者還在點舊卡片：提示重來，不誤查天氣、不誤存訂閱.
		if !isPickerTap(text) {
			return false
		}
		_ = h.line.ReplyText(ctx, replyToken, "地區選擇已逾時（超過 5 分鐘沒有動作），請重新開始",
			models.MessageItem("開啟天氣特報", keywordWeatherWarningOn))
	}
	return true
}

// bareLogCommands [F34-60] 單獨一個「記錄／登錄」的封閉集合（恰好 12 句；比對前先 trimBareLogSuffix）.
// 這些句子沒有內容可記，規則與意圖 AI 都接不住，會落到問答而被誤答「已登錄」；改回固定指引，不靠 AI.
var bareLogCommands = map[string]bool{
	"記錄": true, "登錄": true, "記錄一下": true, "登錄一下": true,
	"幫我記錄": true, "幫我登錄": true, "幫我記錄一下": true, "幫我登錄一下": true,
	"記錄進去": true, "登錄進去": true, "幫我記錄進去": true, "幫我登錄進去": true,
}

// bareLogGuide [F34-60] 單字記錄指令的固定指引.
const bareLogGuide = "要記錄的話，直接傳吃了什麼或做了什麼運動就可以，例如「午餐吃了牛肉麵」「跑步 5公里 30分鐘」"

// bareLogParticles [F34-60] 句尾語助詞（精確 6 字）.
const bareLogParticles = "喔唷啊吧哦啦"

// isBareLogTail [F34-60] 句尾可以去掉的字元：空白、標點（P*）、語助詞、符號類（S*：表情、～）與 emoji 序列組成字元（變體選擇符、零寬連字、外框）.
func isBareLogTail(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) ||
		strings.ContainsRune(bareLogParticles, r) || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf)
}

// trimBareLogSuffix [F34-60] 只從句尾一次去一個字元，到尾字元不符合為止；句首與句中的字元一律保留.
func trimBareLogSuffix(text string) string {
	rs := []rune(text)
	end := len(rs)
	for end > 0 && isBareLogTail(rs[end-1]) {
		end--
	}
	return string(rs[:end])
}

// isBareLogCommand [F34-60] 去尾後與 bareLogCommands 之一全等.
func isBareLogCommand(text string) bool { return bareLogCommands[trimBareLogSuffix(text)] }

// handleBareLogKeyword [F34-60] 去尾後與 bareLogCommands 之一全等才回指引（沒命中時原文照舊往下，不傳去尾後的字串）；
// 不依賴 mealRepo、nutrition、intent，AI 全停用也成立.
func (h *WebhookHandler) handleBareLogKeyword(ctx context.Context, _, text, replyToken string) bool {
	if !isBareLogCommand(text) {
		return false
	}
	_ = h.line.ReplyText(ctx, replyToken, bareLogGuide, foodQuickReplies()[:2]...)
	return true
}
