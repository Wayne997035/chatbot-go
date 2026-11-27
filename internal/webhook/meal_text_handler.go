package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/recentmeal"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	// dupWindow [F34-24] 同一則記餐文字在這段時間內再送一次視為重送（與 photoMergeWindow 同值但各自一個）.
	dupWindow = 10 * time.Minute
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

const (
	// planReply [F34-20] 願望／計畫句的固定回覆.
	planReply = "看起來還沒吃喔。吃完再傳給我，我再幫你記錄（例如「午餐吃了牛肉麵」）。"
	// batchEditMessage [F34-23] 剛一次記了多餐時，份量、店家、再加品項都不改任何一筆.
	batchEditMessage = "剛剛一次記了多餐，份量、店家請到「我的統計」逐筆修改，或輸入「取消這筆」整批取消"
	// droppedMealsNote [F34-21] 一次超過 5 餐時的明講（回覆末行）.
	droppedMealsNote = "（一次最多記 5 餐，後面的請另外再傳一次）"
	// failedSaveNote 某一筆寫入失敗時的標記.
	failedSaveNote = "（這筆暫時無法儲存，請稍後再試）"
	// compactReplyLen [F34-22] 多餐回覆超過這個字數(LINE 的計法,[F44-04])改精簡格式（LINE 單則上限 5000 字）.
	compactReplyLen = 4500
)

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
		keywordTodayMeals, keywordDeleteMeal, keywordCancelMeal, keywordMealPlace,
		keywordFixThis: // [F34-64] 「修正這筆」也算選單指令：追問中點它視為換話題、武裝期間再按一次重新武裝
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
	// [F34-20] 願望／計畫句（「我想吃牛肉麵」「明天中午要吃便當」）不記：不呼叫 AI、不計額度、不開狀態；位置在 AI 之前.
	if nutrition.LooksLikePlan(text) {
		_ = h.line.ReplyText(ctx, replyToken, planReply)
		return true
	}
	if h.nutrition == nil {
		if !explicit {
			return false
		}
		_ = h.line.ReplyText(ctx, replyToken, "飲食估算功能目前未啟用")
		return true
	}
	// [F34-24] 10 分鐘內重送同一則：提醒已記過、不重複記；同樣在 AI 之前，不花估算額度.
	if h.replyIfResend(ctx, userID, text, replyToken) {
		return true
	}
	// [F36-01] 這則訊息問答前的 AI 額度用完：靜默跳過（回 false 往下走），不先扣 mealtext 每日計數、不回「目前無法估算」.
	if !ai.CanSpend(ctx, false) {
		return false
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
		h.saveTextMeals(ctx, userID, replyToken, text, est)
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
	// [F241-10] 對話狀態含餐點原文：刪除之後才處理到的追問不寫
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	state := &conversation.State{Type: conversation.TypeMealClarify, Meal: &conversation.PendingMeal{
		// [F34-27] 追問的原文要留完整（多餐訊息 100 字會丟掉後面的餐）；存進 meal 的 Note 仍截 maxNoteRunes.
		Text: truncateRunes(text, maxInputRunes), MealType: string(est.MealType), DayOffset: est.DayOffset,
	}}
	if !h.convManager.Available() {
		_ = h.line.ReplyText(ctx, replyToken, est.Clarify.Question+"\n請直接打完整的描述，例如「午餐吃了"+est.Clarify.Options[0]+"」")
		return
	}
	setErr := h.convManager.Set(ctx, userID, state)
	// [F241-10][F291-01][F291-03] 寫後再查：檢查與寫入之間完成的刪除，這裡看得到標記，清掉剛寫的狀態；
	// 補刪用會回傳錯誤的 Delete（clearState 吞錯，補刪失敗時會謊稱已刪除）.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if setErr != nil {
		_ = h.line.ReplyText(ctx, replyToken, est.Clarify.Question+"\n請直接打完整的描述，例如「午餐吃了"+est.Clarify.Options[0]+"」")
		return
	}
	items := make([]models.QuickReplyItem, 0, len(est.Clarify.Options)+2)
	for _, o := range est.Clarify.Options {
		// [F24-87] label 截到 LINE 快速回覆上限，text 保留完整選項
		items = append(items, models.MessageItem(truncateLabel(o, maxQuickReplyLabelLen), o))
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
	// [F34-27] 追問前說過的餐別與日期只在重估後剛好一餐時補上（多餐時頂層餐別不知道指哪一餐，各餐用 AI 自己回的）.
	if len(est.Meals) == 1 {
		if est.Meals[0].MealType == "" {
			est.Meals[0].MealType = nutrition.MealType(p.MealType)
		}
		if est.Meals[0].DayOffset == 0 {
			est.Meals[0].DayOffset = p.DayOffset
		}
	}
	h.saveTextMeals(ctx, userID, replyToken, combined, est)
	return true
}

// textMealEntry [F34-22] 一則訊息裡的一餐：要寫的紀錄與顯示用標籤（含「昨天的」）.
type textMealEntry struct {
	m     *meal.Meal
	label string
}

// mealOrder 同一時刻時的排序（依 mealClock 的先後）.
var mealOrder = map[nutrition.MealType]int{
	nutrition.Breakfast: 0, nutrition.Lunch: 1, nutrition.Snack: 2, nutrition.Dinner: 3, nutrition.LateNight: 4,
}

// saveTextMeals [F34-22] 儲存打字記錄的所有餐：一餐一筆，各自餐別與用餐時間；只有一餐時走原本的 saveMeal（回覆逐字不變）.
func (h *WebhookHandler) saveTextMeals(ctx context.Context, userID, replyToken, text string, est *nutrition.TextEstimate) {
	if len(est.Meals) == 0 {
		return
	}
	now := h.dailyNow() // 不用 time.Now()：測試才能釘時鐘
	note := truncateRunes(text, maxNoteRunes)
	resolved := nutrition.ResolveMeals(now, est.Meals)
	entries := make([]textMealEntry, 0, len(resolved))
	for i := range resolved {
		r := &resolved[i]
		eatenAt := nutrition.MealTime(now, r.MealType, r.DayOffset)
		// [F35-03] 點心只有原文含「下午」才放典型時間 15:30，其餘（說點心、或兩個詞都沒說）記在現在.
		if r.Inferred || (r.MealType == nutrition.Snack && !nutrition.SnackAtTeaTime(text)) {
			eatenAt = nutrition.MealTime(now, "", r.DayOffset) // 沒說餐別：沿用單餐舊行為，記在現在
		}
		entries = append(entries, textMealEntry{
			m: &meal.Meal{
				UserID: userID, EatenAt: eatenAt, Items: r.Foods, Total: r.Total, Advice: r.Advice,
				Source: meal.SourceText, MealType: string(r.MealType), Note: note, Place: r.Place, CreateTime: now,
			},
			label: dayPrefix(eatenAt, now) + r.MealType.LabelAt(eatenAt), // [F35-01] 名稱依這筆實際的 EatenAt
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].m, entries[j].m
		if !a.EatenAt.Equal(b.EatenAt) {
			return a.EatenAt.Before(b.EatenAt)
		}
		return mealOrder[nutrition.MealType(a.MealType)] < mealOrder[nutrition.MealType(b.MealType)]
	})
	fp := nutrition.TextFingerprint(text)
	if len(entries) == 1 {
		header := "🍜 已記錄" + entries[0].label + "（文字估算，拍照會更準）"
		h.saveMealFP(ctx, userID, replyToken, entries[0].m, header, fp)
		return
	}
	h.saveMealBatch(ctx, userID, replyToken, entries, fp, est.Dropped)
}

// saveMealBatch [F34-22] 一則訊息多餐 → 多筆.
// 刪除資料的競態：寫前查一次（第一筆之前）、寫後只查一次且放在最後一筆與 recent 之後；補刪涵蓋本次所有嘗試過的 ID
// （Insert 回錯的那筆可能已落地，ID 在寫入前就設好）加上 recent.
func (h *WebhookHandler) saveMealBatch(
	ctx context.Context, userID, replyToken string, entries []textMealEntry, fp string, dropped int,
) {
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	attempted := make([]string, 0, len(entries))
	saved := make([]string, 0, len(entries))
	failed := make([]bool, len(entries))
	for i := range entries {
		m := entries[i].m
		err := h.mealRepo.Insert(ctx, m)
		if m.ID != "" {
			attempted = append(attempted, m.ID)
		}
		if err != nil {
			failed[i] = true
			slog.Error("save meal", "error", redact.Error(err))
			continue
		}
		saved = append(saved, m.ID)
	}
	if len(saved) > 0 {
		h.recent.RememberWithFP(ctx, userID, saved, meal.SourceText, fp)
	}
	undo := func(cctx context.Context) error {
		steps := make([]func(context.Context) error, 0, len(attempted)+1)
		for _, id := range attempted {
			steps = append(steps, func(c context.Context) error {
				_, err := h.mealRepo.DeleteByID(c, userID, id)
				return err
			})
		}
		if len(saved) > 0 {
			steps = append(steps, func(c context.Context) error { return h.recent.Forget(c, userID) })
		}
		return joinUndo(cctx, steps...)
	}
	if h.guardAfterWrite(ctx, userID, replyToken, undo) {
		return
	}
	if len(saved) == 0 {
		_ = h.line.ReplyText(ctx, replyToken, batchReply(entries, failed, false)+"\n"+failedSaveNote, foodQuickReplies()[:2]...)
		return
	}
	reply := batchReply(entries, failed, false)
	tail := h.todayTotalLine(ctx, userID)
	if dropped > 0 {
		tail += "\n" + droppedMealsNote
	}
	if needsCompactReply(reply + tail) {
		reply = batchReply(entries, failed, true)
	}
	// [F34-62] 批次回覆加「修正這筆」：按鈕不指定哪一筆，多筆時由規劃器問「要改哪一筆」.
	items := []models.QuickReplyItem{models.MessageItem(keywordCancelMeal, keywordCancelMeal)}
	items = append(items, h.fixThisButtons()...)
	items = append(items, models.MessageItem("今日飲食", keywordTodayMeals), models.CameraItem("拍下一餐"))
	_ = h.line.ReplyText(ctx, replyToken, reply+tail, items...)
}

// batchReply 多餐回覆：首行「共 N 餐」、逐餐 【標籤】區塊（空一行隔開）；compact 時每餐只留標題行與「合計」行.
func batchReply(entries []textMealEntry, failed []bool, compact bool) string {
	labels := make([]string, len(entries))
	blocks := make([]string, len(entries))
	for i := range entries {
		labels[i] = entries[i].label
		block := nutrition.FormatRecorded(entries[i].m, "【"+entries[i].label+"】")
		if compact {
			block = compactBlock(block)
		}
		if failed[i] {
			block += "\n" + failedSaveNote
		}
		blocks[i] = block
	}
	header := fmt.Sprintf("🍜 已記錄%s（共 %d 餐，文字估算，拍照會更準）", strings.Join(labels, "、"), len(entries))
	return header + "\n\n" + strings.Join(blocks, "\n\n")
}

// compactBlock 只留 FormatRecorded 輸出的標題行與「合計」行.
func compactBlock(block string) string {
	lines := strings.Split(block, "\n")
	keep := []string{lines[0]}
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "合計：") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// replyIfResend [F34-24] 10 分鐘內送出正規化後逐字相同的記餐文字：回「剛剛已經記過這則了」並回 true；不寫入、不動 recent（At 不刷新）.
// 三個條件全成立才算重送：指紋相同、距第一次記錄 ≤ dupWindow、recent 的紀錄至少一筆還在（被 LIFF 刪光就照常記）.
func (h *WebhookHandler) replyIfResend(ctx context.Context, userID, text, replyToken string) bool {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok || recent.Fp == "" || recent.Fp != nutrition.TextFingerprint(text) || h.recent.Now().Sub(recent.At) > dupWindow {
		return false
	}
	var labels []string
	now := h.dailyNow()
	for _, id := range recentIDs(&recent) {
		m, err := h.mealRepo.FindByID(ctx, userID, id)
		if err != nil || m == nil {
			continue
		}
		labels = append(labels, dayPrefix(m.EatenAt, now)+nutrition.MealType(m.MealType).LabelAt(m.EatenAt)) // [F35-01]
	}
	if len(labels) == 0 {
		return false
	}
	reply := "剛剛已經記過這則了（" + strings.Join(labels, "、") + "）。要改的話，直接說要改哪一餐。" + h.todayTotalLine(ctx, userID)
	_ = h.line.ReplyText(ctx, replyToken, reply,
		models.MessageItem("今日飲食", keywordTodayMeals), models.MessageItem(keywordCancelMeal, keywordCancelMeal))
	return true
}

// recentIDs recent 指標涵蓋的所有紀錄 ID（批次有 IDs，單筆只有 ID）.
func recentIDs(r *recentmeal.Meal) []string {
	if len(r.IDs) > 1 {
		return r.IDs
	}
	return []string{r.ID}
}

// isBatch [F34-23] recent 指到的是一次記的多餐.
func isBatch(r *recentmeal.Meal) bool { return len(r.IDs) > 1 }

// replyBatchEdit [F34-23] 批次不能逐筆改份量、店家、加品項：固定回覆，不改任何一筆；有 LIFF 附「看圖表」.
func (h *WebhookHandler) replyBatchEdit(ctx context.Context, replyToken string) {
	_ = h.line.ReplyText(ctx, replyToken, batchEditMessage,
		h.withChartButton([]models.QuickReplyItem{models.MessageItem(keywordCancelMeal, keywordCancelMeal)})...)
}

// saveMeal 儲存一餐並回覆內容、今日累計與後續操作按鈕；沒有資料庫時只回覆分析.
func (h *WebhookHandler) saveMeal(ctx context.Context, userID, replyToken string, m *meal.Meal, header string) {
	h.saveMealFP(ctx, userID, replyToken, m, header, "")
}

// saveMealFP 同 saveMeal；fp 是這則文字的指紋（打字記餐才有，照片等傳空字串）.
func (h *WebhookHandler) saveMealFP(ctx context.Context, userID, replyToken string, m *meal.Meal, header, fp string) {
	reply := nutrition.FormatRecorded(m, header)
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, reply, foodQuickReplies()[:2]...)
		return
	}
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	insertErr := h.mealRepo.Insert(ctx, m)
	if insertErr == nil {
		h.recent.RememberWithFP(ctx, userID, []string{m.ID}, m.Source, fp)
	}
	// [F291-04] 寫後再查只做一次，放在最後一個寫入（最近一餐指標）之後：補刪只刪這次寫的那一筆（Insert 在寫入前就設好 ID），
	// 刪除後新記的餐不受影響；Insert 回錯也查（可能已落地），此時沒寫最近一餐指標，不動它.
	undoMeal := func(cctx context.Context) error {
		_, err := h.mealRepo.DeleteByID(cctx, userID, m.ID)
		return err
	}
	undo := undoMeal
	if insertErr == nil {
		undo = func(cctx context.Context) error {
			return joinUndo(cctx, undoMeal, func(c context.Context) error { return h.recent.Forget(c, userID) })
		}
	}
	if h.guardAfterWrite(ctx, userID, replyToken, undo) {
		return
	}
	if insertErr != nil {
		slog.Error("save meal", "error", redact.Error(insertErr))
		_ = h.line.ReplyText(ctx, replyToken, reply+"\n（這筆暫時無法儲存，請稍後再試）", foodQuickReplies()[:2]...)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, reply+h.todayTotalLine(ctx, userID), h.mealActions(m)...)
}

// mealActions 剛記錄（或剛修改）的一餐下方的按鈕.
// [F34-62] 不再放「只吃一半」「大份」（打字份量指令照舊有效）；聊天改紀錄啟用時放「修正這筆」.
func (h *WebhookHandler) mealActions(m *meal.Meal) []models.QuickReplyItem {
	var items []models.QuickReplyItem
	if m.CurrentScale() != 1 {
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
	items = append(items, models.MessageItem(keywordCancelMeal, keywordCancelMeal))
	items = append(items, h.fixThisButtons()...)
	return append(items, models.MessageItem("今日飲食", keywordTodayMeals))
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
	if isBatch(&recent) { // [F34-23]
		h.replyBatchEdit(ctx, replyToken)
		return
	}
	m := h.modifyMeal(ctx, userID, replyToken, recent.ID, func(m *meal.Meal) { m.SetScale(scale) })
	if m == nil {
		return
	}
	reply := nutrition.FormatRecorded(m, "👌 已改成"+portionLabel(scale)) + h.todayTotalLine(ctx, userID)
	_ = h.line.ReplyText(ctx, replyToken, reply, h.mealActions(m)...)
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
	// [F34-23] 批次逐筆刪（recentIDs 單筆時只有 recent.ID）；全處理完才 Forget，任一筆報錯就不 Forget 並請使用者重試.
	deletedCount := 0
	var firstErr error
	for _, id := range recentIDs(&recent) {
		deleted, err := h.mealRepo.DeleteByID(ctx, userID, id)
		if err != nil {
			slog.Error("cancel recent meal", "error", err)
			firstErr = err
			continue
		}
		if deleted {
			deletedCount++
		}
	}
	if firstErr != nil {
		_ = h.line.ReplyText(ctx, replyToken, "刪除失敗，請稍後再試")
		return
	}
	if err := h.recent.Forget(ctx, userID); err != nil {
		slog.Warn("forget recent meal", "error", err)
	}
	if deletedCount == 0 {
		_ = h.line.ReplyText(ctx, replyToken, "這筆已經刪除了", foodQuickReplies()[2:3]...)
		return
	}
	if isBatch(&recent) {
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("已取消這批紀錄（共 %d 筆）", deletedCount)+h.todayTotalLine(ctx, userID), foodQuickReplies()...)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, "已取消這筆紀錄"+h.todayTotalLine(ctx, userID), foodQuickReplies()...)
}

// addToRecentMeal 「再加一顆滷蛋」：加到剛記的那餐；30 分鐘內沒有記錄就記成新的一筆.
func (h *WebhookHandler) addToRecentMeal(ctx context.Context, userID, food, replyToken string) {
	// [F34-20] 「再加明天的便當」是計畫句：同 handleTextMeal，不呼叫 AI、不改任何一筆.
	if nutrition.LooksLikePlan(food) {
		_ = h.line.ReplyText(ctx, replyToken, planReply)
		return
	}
	recent, ok := h.recent.Get(ctx, userID)
	if !ok || h.nutrition == nil {
		h.handleTextMeal(ctx, userID, food, replyToken, true)
		return
	}
	if isBatch(&recent) { // [F34-23]
		h.replyBatchEdit(ctx, replyToken)
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
		_ = h.line.ReplyText(ctx, replyToken, "看不出要加什麼，可以再說清楚一點", h.mealActions(m)...)
		return
	}
	// 估算要幾秒，期間可能在 LIFF 改了份量或記了店家：在最新的資料上加.
	if m = h.modifyMeal(ctx, userID, replyToken, recent.ID, func(m *meal.Meal) {
		m.Rebase()
		for i := range est.Meals { // [F32-02] 所有餐的食物攤平加進最近一餐
			m.Items = append(m.Items, est.Meals[i].Foods...)
		}
		m.Recalc()
	}); m == nil {
		return
	}
	header := "➕ 已加到剛剛的" + nutrition.MealType(m.MealType).LabelAt(m.EatenAt) // [F35-01]
	_ = h.line.ReplyText(ctx, replyToken, nutrition.FormatRecorded(m, header)+h.todayTotalLine(ctx, userID), h.mealActions(m)...)
}

// askMealPlace 問剛記的那餐在哪吃；可以輸入店名，或用 LINE 地圖選店家.
func (h *WebhookHandler) askMealPlace(ctx context.Context, userID, replyToken string) {
	recent, ok := h.recent.Get(ctx, userID)
	if !ok {
		h.replyNoRecentMeal(ctx, replyToken)
		return
	}
	if isBatch(&recent) { // [F34-23]
		h.replyBatchEdit(ctx, replyToken)
		return
	}
	state := &conversation.State{Type: conversation.TypeMealPlace, Meal: &conversation.PendingMeal{MealID: recent.ID}}
	if !h.convManager.Available() {
		_ = h.line.ReplyText(ctx, replyToken, "目前無法記錄店家，請稍後再試")
		return
	}
	setErr := h.convManager.Set(ctx, userID, state)
	// [F291-03] 只含 mealID 的狀態也要清：刪除後殘留的狀態會讓下一則文字被當成店名寫回這餐.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if setErr != nil {
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
	err := h.convManager.Set(ctx, userID, state)
	// [F291-03] 同 askMealPlace.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if err != nil {
		slog.Warn("re-ask meal place", "error", redact.Error(err))
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

// needsCompactReply [F44-04] 多餐回覆全文超過精簡門檻(以 LINE 的 UTF-16 計法).
func needsCompactReply(full string) bool { return models.TextLen(full) > compactReplyLen }
