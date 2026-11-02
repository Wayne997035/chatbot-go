package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 打字記餐與「剛記錄那一餐」的操作.
const (
	keywordHalf       = "只吃一半"
	keywordLarge      = "大份"
	keywordDouble     = "兩份"
	keywordOriginal   = "原本份量"
	keywordCancelMeal = "取消這筆"
	keywordMealPlace  = "在哪吃的"
	keywordSkipPlace  = "略過"

	// textMealDailyLimit 每人每天打字記餐的 AI 估算次數（免費供應商），避免額度被少數人用光.
	textMealDailyLimit = 60
	// textMealTimeout 文字估算的時間上限.
	textMealTimeout = 25 * time.Second
	// textMealLoadingSeconds 文字估算時「輸入中」動畫秒數（LINE 限 5 的倍數）.
	textMealLoadingSeconds = 20
	// maxNoteRunes 保存使用者原文的長度上限.
	maxNoteRunes = 100
	// maxClarifyAnswerRunes 追問時超過這個長度的回覆視為新話題.
	maxClarifyAnswerRunes = 30
	// maxPlaceNameRunes / maxPlaceAddressRunes 店家資訊的長度上限.
	maxPlaceNameRunes    = 30
	maxPlaceAddressRunes = 100
)

// portionScales 份量指令（已去掉空白、標點與語尾）→ 相對於估算的倍數（不是在目前份量上再乘）.
var portionScales = map[string]float64{
	keywordHalf: 0.5, "吃一半": 0.5, "半份": 0.5, "只吃了一半": 0.5, "吃了一半": 0.5, "我只吃一半": 0.5, "我只吃了一半": 0.5,
	keywordLarge: 1.5, "大碗": 1.5, "大份量": 1.5, "是大碗": 1.5, "大碗的": 1.5, "是大份": 1.5,
	keywordDouble: 2, "2份": 2, "兩人份": 2,
	keywordOriginal: 1, "原本的份量": 1,
}

// portionParticles 份量指令後面常見的語尾，比對前去掉（「只吃一半啦」「大碗喔」）.
var portionParticles = []string{"啦", "喔", "哦", "唷", "耶", "而已", "了", "的說", "的"}

// portionScale 寬鬆比對份量指令：去掉空白、標點、表情符號後查表，查不到就去掉一個語尾再查.
func portionScale(text string) (float64, bool) {
	s := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return r
	}, text)
	for s != "" {
		if scale, ok := portionScales[s]; ok {
			return scale, true
		}
		trimmed := s
		for _, p := range portionParticles {
			if t, ok := strings.CutSuffix(s, p); ok {
				trimmed = t
				break
			}
		}
		if trimmed == s {
			break
		}
		s = trimmed
	}
	return 0, false
}

// logPrefixes 明確要記成一餐（AI 問答的「記成一餐」按鈕也送這個）.
var logPrefixes = []string{"記錄：", "記錄:", "記錄 "}

// addPrefixes 在剛記的那餐再加品項.
var addPrefixes = []string{"再加", "另外加", "還有加"}

var cancelWords = map[string]bool{"取消": true, "不用了": true, "算了": true, keywordSkipPlace: true}

// genericPlaceTitles 沒選店家、只放圖釘時 LINE 位置訊息的預設標題，不是店名.
var genericPlaceTitles = map[string]bool{"位置資訊": true, "位置": true, "我的位置": true, "Location": true, "Location information": true}

// mealLogText 「記成一餐」按鈕送出的文字.
func mealLogText(food string) string { return logPrefixes[0] + food }

// isMenuCommand 圖片選單與主要功能的文字；追問中點了這些視為換話題.
func isMenuCommand(text string) bool {
	switch text {
	case keywordWeather, keywordSnapFood, keywordLogWorkout, keywordToday, keywordWeek, keywordAsk,
		keywordTodayMeals, keywordDeleteMeal, keywordCancelMeal, keywordMealPlace:
		return true
	}
	return helpAliases[text]
}

// handleMealCommand 份量、取消、店家、明確記錄與加點；回傳 true 表示已處理.
func (h *WebhookHandler) handleMealCommand(ctx context.Context, userID, text, replyToken string) bool {
	if h.mealRepo == nil {
		return false
	}
	if scale, ok := portionScale(text); ok {
		h.adjustPortion(ctx, userID, replyToken, scale)
		return true
	}
	switch text {
	case keywordCancelMeal:
		h.cancelRecentMeal(ctx, userID, replyToken)
		return true
	case keywordMealPlace:
		h.askMealPlace(ctx, userID, replyToken)
		return true
	}
	if food, ok := cutAnyPrefix(text, logPrefixes); ok {
		if fitness.LooksLikeWorkout(food) && !nutrition.LooksLikeMealLog(food) {
			return false // 「記錄 跑步 5公里」交給運動紀錄
		}
		return h.handleTextMeal(ctx, userID, food, replyToken, true)
	}
	if food, ok := cutAnyPrefix(text, addPrefixes); ok && !strings.HasPrefix(food, "油") {
		h.addToRecentMeal(ctx, userID, food, replyToken)
		return true
	}
	return false
}

func cutAnyPrefix(text string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(text, p); ok {
			if rest = strings.TrimSpace(rest); rest != "" {
				return rest, true
			}
		}
	}
	return "", false
}

// handleTextMeal 打字記餐. explicit 表示使用者明確要記錄（「記錄：」）；否則只處理像在描述吃了什麼的句子.
// 回傳 true 表示已處理.
func (h *WebhookHandler) handleTextMeal(ctx context.Context, userID, text, replyToken string, explicit bool) bool {
	if h.mealRepo == nil || (!explicit && !nutrition.LooksLikeMealLog(text)) {
		return false
	}
	if h.nutrition == nil {
		if !explicit {
			return false
		}
		_ = h.line.ReplyText(ctx, replyToken, "飲食估算功能目前未啟用")
		return true
	}
	est, ok := h.estimateText(ctx, userID, text, replyToken, true)
	if !ok {
		return true
	}
	switch {
	case !est.IsFood && explicit:
		_ = h.line.ReplyText(ctx, replyToken, "看不出吃了什麼，可以再說清楚一點，或直接拍照給我", foodQuickReplies()[:2]...)
	case !est.IsFood:
		return false // 例如「吃了閉門羹」，交給後面的流程
	case est.Clarify != nil:
		h.askMealClarify(ctx, userID, replyToken, text, est)
	default:
		h.saveTextMeal(ctx, userID, replyToken, text, est)
	}
	return true
}

// estimateText 呼叫 AI 估算；失敗或超過上限時已回覆使用者並回傳 false.
func (h *WebhookHandler) estimateText(
	ctx context.Context, userID, text, replyToken string, allowClarify bool,
) (*nutrition.TextEstimate, bool) {
	if !h.withinDailyLimit(ctx, "mealtext", userID, textMealDailyLimit) {
		_ = h.line.ReplyText(ctx, replyToken, "今天的飲食估算次數已達上限，明天再來喔！")
		return nil, false
	}
	if err := h.line.StartLoading(ctx, userID, textMealLoadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}
	actx, cancel := context.WithTimeout(ctx, textMealTimeout)
	defer cancel()
	est, err := h.nutrition.EstimateText(actx, text, allowClarify)
	if err != nil {
		slog.Warn("estimate text meal", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "目前無法估算，可以拍照給我，或稍後再試", foodQuickReplies()[:2]...)
		return nil, false
	}
	return est, true
}

// askMealClarify 描述太籠統（只說「便當」）時追問；也可以直接補照片.
func (h *WebhookHandler) askMealClarify(ctx context.Context, userID, replyToken, text string, est *nutrition.TextEstimate) {
	state := &conversation.State{Type: conversation.TypeMealClarify, Meal: &conversation.PendingMeal{
		Text: truncateRunes(text, maxNoteRunes), MealType: string(est.MealType), DayOffset: est.DayOffset,
	}}
	if !h.convManager.Available() || h.convManager.Set(ctx, userID, state) != nil {
		_ = h.line.ReplyText(ctx, replyToken, est.Clarify.Question+"\n請直接打完整的描述，例如「午餐吃了"+est.Clarify.Options[0]+"」")
		return
	}
	items := make([]models.QuickReplyItem, 0, len(est.Clarify.Options)+2)
	for _, o := range est.Clarify.Options {
		items = append(items, models.MessageItem(o, o))
	}
	items = append(items, models.CameraItem("拍給我看"), models.MessageItem("取消", "取消"))
	_ = h.line.ReplyText(ctx, replyToken, est.Clarify.Question, items...)
}

// handleMealConversation 追問與店家的回覆；回傳 false 表示不是這兩種狀態，或使用者換了話題.
func (h *WebhookHandler) handleMealConversation(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) bool {
	switch state.Type {
	case conversation.TypeMealClarify:
		return h.answerMealClarify(ctx, userID, text, replyToken, state.Meal)
	case conversation.TypeMealPlace:
		return h.answerMealPlace(ctx, userID, text, replyToken, state.Meal)
	default:
		return false
	}
}

// looksLikeNewTopic 追問中收到的訊息其實是新的話題（點了選單、問問題、或一大段話）.
func looksLikeNewTopic(text string, maxRunes int) bool {
	return isMenuCommand(text) || nutrition.LooksLikeQuestion(text) || utf8.RuneCountInString(text) > maxRunes
}

func (h *WebhookHandler) answerMealClarify(ctx context.Context, userID, text, replyToken string, p *conversation.PendingMeal) bool {
	h.clearState(ctx, userID)
	if cancelWords[text] {
		_ = h.line.ReplyText(ctx, replyToken, "好，這餐先不記錄")
		return true
	}
	if p == nil || h.nutrition == nil || looksLikeNewTopic(text, maxClarifyAnswerRunes) {
		return false
	}
	combined := p.Text + "（補充：" + text + "）"
	est, ok := h.estimateText(ctx, userID, combined, replyToken, false)
	if !ok {
		return true
	}
	if !est.IsFood {
		_ = h.line.ReplyText(ctx, replyToken, "看不出吃了什麼，可以再說清楚一點，或直接拍照給我", foodQuickReplies()[:2]...)
		return true
	}
	if est.MealType == "" {
		est.MealType = nutrition.MealType(p.MealType)
	}
	if est.DayOffset == 0 {
		est.DayOffset = p.DayOffset
	}
	h.saveTextMeal(ctx, userID, replyToken, combined, est)
	return true
}

// saveTextMeal 儲存打字記錄的一餐.
func (h *WebhookHandler) saveTextMeal(ctx context.Context, userID, replyToken, text string, est *nutrition.TextEstimate) {
	now := time.Now()
	mealType := est.MealType
	eatenAt := nutrition.MealTime(now, mealType, est.DayOffset)
	if mealType == "" {
		mealType = nutrition.MealTypeAt(eatenAt)
	}
	m := &meal.Meal{
		UserID: userID, EatenAt: eatenAt, Items: est.Foods, Total: est.Total, Advice: est.Advice,
		Source: meal.SourceText, MealType: string(mealType), Note: truncateRunes(text, maxNoteRunes), Place: est.Place, CreateTime: now,
	}
	header := "🍜 已記錄" + dayPrefix(eatenAt, now) + mealType.Label() + "（文字估算，拍照會更準）"
	h.saveMeal(ctx, userID, replyToken, m, header)
}

// saveMeal 儲存一餐並回覆內容、今日累計與後續操作按鈕；沒有資料庫時只回覆分析.
func (h *WebhookHandler) saveMeal(ctx context.Context, userID, replyToken string, m *meal.Meal, header string) {
	reply := nutrition.FormatRecorded(m, header)
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, reply, foodQuickReplies()[:2]...)
		return
	}
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	if err := h.mealRepo.Insert(ctx, m); err != nil {
		slog.Error("save meal", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, reply+"\n（這筆暫時無法儲存，請稍後再試）", foodQuickReplies()[:2]...)
		return
	}
	h.recent.Remember(ctx, userID, m.ID, m.Source)
	_ = h.line.ReplyText(ctx, replyToken, reply+h.todayTotalLine(ctx, userID), mealActions(m)...)
}

// mealActions 剛記錄（或剛修改）的一餐下方的按鈕.
func mealActions(m *meal.Meal) []models.QuickReplyItem {
	var items []models.QuickReplyItem
	if m.CurrentScale() == 1 {
		items = append(items, models.MessageItem(keywordHalf, keywordHalf), models.MessageItem(keywordLarge, keywordLarge))
	} else {
		items = append(items, models.MessageItem(keywordOriginal, keywordOriginal))
	}
	if m.Source == meal.SourceText {
		items = append(items, models.CameraItem("補照片"))
	} else {
		items = append(items, models.CameraItem("拍下一餐"))
	}
	if m.Place == nil {
		items = append(items, models.MessageItem(keywordMealPlace, keywordMealPlace))
	}
	return append(items, models.MessageItem(keywordCancelMeal, keywordCancelMeal), models.MessageItem("今日飲食", keywordTodayMeals))
}

// todayTotalLine 「今日累計」一行；讀取失敗時回傳空字串.
func (h *WebhookHandler) todayTotalLine(ctx context.Context, userID string) string {
	meals, err := h.todayMeals(ctx, userID)
	if err != nil {
		slog.Warn("find today meals", "error", err)
		return ""
	}
	total := meal.Sum(meals)
	return fmt.Sprintf("\n今日累計：%.0f 大卡（%d 餐）", total.Calories, len(meals))
}

// dayPrefix 不是今天的餐加上「昨天的」「前天的」.
func dayPrefix(eatenAt, now time.Time) string {
	today, _ := nutrition.DayRange(now)
	day, _ := nutrition.DayRange(eatenAt)
	switch int(today.Sub(day).Hours() / 24) {
	case 1:
		return "昨天的"
	case 2:
		return "前天的"
	default:
		return ""
	}
}

// replyNoRecentMeal 30 分鐘內沒有剛記的餐；有 LIFF 才建議到統計頁修改.
func (h *WebhookHandler) replyNoRecentMeal(ctx context.Context, replyToken string) {
	if h.liffURL != "" {
		_ = h.line.ReplyText(ctx, replyToken, "找不到 30 分鐘內記錄的餐點，可以到「我的統計」修改份量或刪除",
			h.withChartButton(foodQuickReplies()[2:3])...)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "找不到 30 分鐘內記錄的餐點；要刪除最近一筆可以輸入「刪除上一筆」", foodQuickReplies()[2:4]...)
}

// modifyMeal 讀出使用者的一餐、套用 change 再寫回；期間被別的訊息或 LIFF 改過就在最新資料上重做.
// 失敗或找不到時已回覆使用者並回傳 nil.
func (h *WebhookHandler) modifyMeal(ctx context.Context, userID, replyToken, mealID string, change func(*meal.Meal)) *meal.Meal {
	if h.blockedByErasure(ctx, userID, replyToken) {
		return nil
	}
	m, err := meal.Modify(ctx, h.mealRepo, userID, mealID, change)
	switch {
	case errors.Is(err, meal.ErrConflict):
		slog.Warn("update meal: still conflicting after retries")
		_ = h.line.ReplyText(ctx, replyToken, "這筆紀錄剛好在其他地方被修改，請再試一次")
	case err != nil:
		slog.Error("update meal", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "修改失敗，請稍後再試")
	case m == nil:
		h.replyNoRecentMeal(ctx, replyToken)
	}
	return m
}

// adjustPortion 把剛記的那餐改成估算的 scale 倍（scale 來自 portionScales，都在可接受範圍內）.
func (h *WebhookHandler) adjustPortion(ctx context.Context, userID, replyToken string, scale float64) {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok {
		h.replyNoRecentMeal(ctx, replyToken)
		return
	}
	m := h.modifyMeal(ctx, userID, replyToken, recent.ID, func(m *meal.Meal) { m.SetScale(scale) })
	if m == nil {
		return
	}
	reply := nutrition.FormatRecorded(m, "👌 已改成"+portionLabel(scale)) + h.todayTotalLine(ctx, userID)
	_ = h.line.ReplyText(ctx, replyToken, reply, mealActions(m)...)
}

func portionLabel(scale float64) string {
	switch scale {
	case 0.5:
		return "一半的份量"
	case 1:
		return "原本的份量"
	case 2:
		return "兩份"
	default:
		return fmt.Sprintf("%.1f 倍的份量", scale)
	}
}

// cancelRecentMeal 刪除剛記的那餐（不是「最新的一餐」，補記昨天的也刪得到）.
func (h *WebhookHandler) cancelRecentMeal(ctx context.Context, userID, replyToken string) {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok {
		h.replyNoRecentMeal(ctx, replyToken)
		return
	}
	deleted, err := h.mealRepo.DeleteByID(ctx, userID, recent.ID)
	if err != nil {
		slog.Error("cancel recent meal", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "刪除失敗，請稍後再試")
		return
	}
	if err := h.recent.Forget(ctx, userID); err != nil {
		slog.Warn("forget recent meal", "error", err)
	}
	if !deleted {
		_ = h.line.ReplyText(ctx, replyToken, "這筆已經刪除了", foodQuickReplies()[2:3]...)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "已取消這筆紀錄"+h.todayTotalLine(ctx, userID), foodQuickReplies()...)
}

// addToRecentMeal 「再加一顆滷蛋」：加到剛記的那餐；30 分鐘內沒有記錄就記成新的一筆.
func (h *WebhookHandler) addToRecentMeal(ctx context.Context, userID, food, replyToken string) {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok || h.nutrition == nil {
		h.handleTextMeal(ctx, userID, food, replyToken, true)
		return
	}
	m, err := h.mealRepo.FindByID(ctx, userID, recent.ID)
	if err != nil || m == nil {
		h.handleTextMeal(ctx, userID, food, replyToken, true)
		return
	}
	est, ok := h.estimateText(ctx, userID, "吃了"+food, replyToken, false)
	if !ok {
		return
	}
	if !est.IsFood {
		_ = h.line.ReplyText(ctx, replyToken, "看不出要加什麼，可以再說清楚一點", mealActions(m)...)
		return
	}
	// 估算要幾秒，期間可能在 LIFF 改了份量或記了店家：在最新的資料上加.
	if m = h.modifyMeal(ctx, userID, replyToken, recent.ID, func(m *meal.Meal) {
		m.Rebase()
		m.Items = append(m.Items, est.Foods...)
		m.Recalc()
	}); m == nil {
		return
	}
	header := "➕ 已加到剛剛的" + nutrition.MealType(m.MealType).Label()
	_ = h.line.ReplyText(ctx, replyToken, nutrition.FormatRecorded(m, header)+h.todayTotalLine(ctx, userID), mealActions(m)...)
}

// askMealPlace 問剛記的那餐在哪吃；可以輸入店名，或用 LINE 地圖選店家.
func (h *WebhookHandler) askMealPlace(ctx context.Context, userID, replyToken string) {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok {
		h.replyNoRecentMeal(ctx, replyToken)
		return
	}
	state := &conversation.State{Type: conversation.TypeMealPlace, Meal: &conversation.PendingMeal{MealID: recent.ID}}
	if !h.convManager.Available() || h.convManager.Set(ctx, userID, state) != nil {
		_ = h.line.ReplyText(ctx, replyToken, "目前無法記錄店家，請稍後再試")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "在哪間店吃的呢？直接輸入店名，或按「選擇位置」在地圖上點店家",
		models.LocationItem("選擇位置"), models.MessageItem(keywordSkipPlace, keywordSkipPlace))
}

func (h *WebhookHandler) answerMealPlace(ctx context.Context, userID, text, replyToken string, p *conversation.PendingMeal) bool {
	h.clearState(ctx, userID)
	if cancelWords[text] {
		_ = h.line.ReplyText(ctx, replyToken, "好，略過店家", foodQuickReplies()[2:3]...)
		return true
	}
	if p == nil {
		return false
	}
	if nutrition.ContainsURL(text) {
		// 貼了地圖連結：看不出店名，再問一次（狀態保留）.
		h.reaskMealPlace(ctx, userID, replyToken, p, "連結看不出店名，請直接輸入店名，或按「選擇位置」在地圖上點店家")
		return true
	}
	if looksLikeNewTopic(text, maxPlaceNameRunes) {
		return false
	}
	name := nutrition.CleanPlaceName(text)
	if name == "" {
		h.reaskMealPlace(ctx, userID, replyToken, p, "看不出店名，請輸入店名，或按「選擇位置」")
		return true
	}
	h.setMealPlace(ctx, userID, replyToken, p.MealID, &meal.Place{Name: name, Source: meal.PlaceFromUser})
	return true
}

// reaskMealPlace 店名看不懂時再問一次.
func (h *WebhookHandler) reaskMealPlace(ctx context.Context, userID, replyToken string, p *conversation.PendingMeal, message string) {
	state := &conversation.State{Type: conversation.TypeMealPlace, Meal: p}
	if err := h.convManager.Set(ctx, userID, state); err != nil {
		slog.Warn("re-ask meal place", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "目前無法記錄店家，請稍後再試")
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, message, models.LocationItem("選擇位置"), models.MessageItem(keywordSkipPlace, keywordSkipPlace))
}

// handleMealLocation 等店家時收到的位置訊息記成這餐的店家；回傳 false 表示不是在等店家（照常查天氣）.
func (h *WebhookHandler) handleMealLocation(ctx context.Context, userID string, msg *models.Message, replyToken string) bool {
	state, err := h.convManager.Get(ctx, userID)
	if err != nil || state == nil || state.Type != conversation.TypeMealPlace || state.Meal == nil || h.mealRepo == nil {
		return false
	}
	h.clearState(ctx, userID)
	address := strings.Join(strings.Fields(msg.Address), " ")
	if nutrition.ContainsURL(address) {
		address = ""
	}
	place := &meal.Place{Address: truncateRunes(address, maxPlaceAddressRunes), Source: meal.PlaceFromUser}
	if title := strings.TrimSpace(msg.Title); !genericPlaceTitles[title] {
		place.Name = nutrition.CleanPlaceName(truncateRunes(title, maxPlaceNameRunes))
	}
	if place.Name == "" && place.Address == "" {
		_ = h.line.ReplyText(ctx, replyToken, "沒有收到店家資訊，可以直接輸入店名")
		return true
	}
	h.setMealPlace(ctx, userID, replyToken, state.Meal.MealID, place)
	return true
}

func (h *WebhookHandler) setMealPlace(ctx context.Context, userID, replyToken, mealID string, place *meal.Place) {
	if h.modifyMeal(ctx, userID, replyToken, mealID, func(m *meal.Meal) { m.Place = place }) == nil {
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "📍 已記下這餐在「"+nutrition.PlaceLabel(place)+"」", foodQuickReplies()[2:3]...)
}

// truncateRunes 截到 n 個字.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
