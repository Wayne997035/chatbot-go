package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/redact"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// [F33-22] 聊天直接改刪紀錄：觸發（關鍵字快判 + 意圖分類補漏）、規劃、出確認卡 / 候選卡.
// AI 只提議（recordedit.Planner）；暫存在 recordedit.Store；客人按確認（postback）才寫入，見 record_edit_postback.go.

// 修改紀錄的上限與時間預算.
const (
	// editDailyLimit 每人每天可以叫修改規劃器的次數；超過時明講上限（客人改不了又不知道為什麼，等於跳針）.
	editDailyLimit = 40
	// editTimeoutMax 規劃器單次呼叫的逾時上限.
	editTimeoutMax = 10 * time.Second
	// editMinBudget 排完後少於這個時間就不叫規劃器.
	editMinBudget = 2 * time.Second
	// maxEditCandidates 候選按鈕最多幾顆（規格：最多 5 個）.
	maxEditCandidates = 5
	// maxEditSummaryRunes 確認卡文字的保險上限（LINE 文字上限 5000）.
	maxEditSummaryRunes = 1000
	// editEditableDays 「近 7 天有可改紀錄」的天數（與規劃器編號表同窗口）.
	editEditableDays = 7
)

// 回覆文字. 失敗類的回覆一律不含「已改」「已更新」「已刪除」（只有寫入回成功才說）.
const (
	editInvalidMessage     = "我這次沒看懂要改哪一筆或改成多少，可以直接說「把午餐的麵包改成 210 大卡」"
	editNoChangeMessage    = "這筆本來就是這個數字，不用改"
	editLastItemMessage    = "這餐只剩一個品項，要刪整餐請說「刪掉這餐」"
	editUnavailableMessage = "目前無法修改，請稍後再試"
	editExpiredMessage     = "這個修改已過期或已處理過，請重新說一次"
	editCancelledMessage   = "好，沒有修改"
	editFailedMessage      = "修改失敗，請稍後再試"
	editStaleMessage       = "這筆在你確認前被改過，請重新說一次"
	editGoneMessage        = "這筆已經不在了"
	editPickMessage        = "要改哪一筆？"
	// [F34-15] 多筆寫入中途失敗、連撤回也沒完全成功：不含「已改」「已調整」「已更新」，請客人看今日紀錄.
	editPartialMessage = "這次調整沒有完整完成，請輸入「今日」確認目前的紀錄，需要的話再跟我說一次"
	// [F34-17] 打字「確認」「取消」但沒有待確認的修改（含已過期、他人的卡）；確認與取消同一句.
	editNoPendingMessage = "目前沒有等你確認的修改"
	// [F34-17] 打字「確認」時暫存的是候選清單（還沒選是哪一筆）：不消耗、請客人點按鈕.
	editPickFirstMessage = "請先點選要改哪一筆"
)

// 修改動詞與更正語氣（規格 §3a）；命中只代表「值得叫一次規劃器」，不決定改什麼.
var (
	editVerbs = []string{
		"改", "修正", "更正", "更新", "調整", "換成", "刪", "刪除", "刪掉", "拿掉", "去掉", "移除",
		"重算", "重新計算", "重新分析", "重新估", "重估",
		// [F33-41] 客人主動要求把漏掉的加上去、多算的扣掉（「幫我加上去」「幫我補上」「幫我扣掉」以前只走問答）.
		"加上去", "加進去", "算進去", "補上", "補進去", "扣掉", "扣除", "減掉",
		// [F34-18] 改餐別、把品項移到另一餐、拆成多餐（誤中只花一次規劃器，回 none 照舊往下走）.
		"移到", "移去", "移至", "搬到", "分開", "分成", "拆成", "拆開", "歸到",
	}
	editCorrections = []string{
		"算錯", "不對", "不是", "少算", "多算", "漏了", "應該比", "應該是", "其實有", "裡面有", "還有加", "有加",
	}
)

// EditGate [F33-22] 關鍵字快判（確定性、不呼叫 AI）：訊息含修改動詞或更正語氣就回 true.
// 輸入先清理、去空白；超過 maxInputRunes 一律 false.
func EditGate(text string) bool {
	if utf8.RuneCountInString(text) > maxInputRunes { // 先量原文：CleanQuestion 會把過長的字串截短，量清理後的長度會漏掉
		return false
	}
	text = strings.Join(strings.Fields(assistant.CleanQuestion(text)), "")
	if text == "" {
		return false
	}
	for _, w := range editVerbs {
		if strings.Contains(text, w) {
			return true
		}
	}
	for _, w := range editCorrections {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// WithRecordEdit [F33-25] 啟用聊天修改紀錄；store 為 nil = 功能停用（gate 命中也不處理，訊息照舊往下走）.
// 另需 planner（WithAssistant 內設定）；兩者都有才會啟用.
func (h *WebhookHandler) WithRecordEdit(store recordedit.Store) *WebhookHandler {
	h.editStore = store
	return h
}

// recordEditEnabled [F33-26] 功能啟用條件 = planner 與 store 皆非 nil.
func (h *WebhookHandler) recordEditEnabled() bool {
	return h.editStore != nil && h.editPlanner != nil
}

// editTimeout [F33-22] 規劃器的逾時 = min(10 秒, 剩餘時間 − 40 秒問答 − 4 秒回覆餘裕)；少於 2 秒就不叫（剩餘 < 46 秒）.
func editTimeout(remaining time.Duration) (time.Duration, bool) {
	t := min(editTimeoutMax, remaining-assistantWorstCase-replyMargin)
	return t, t >= editMinBudget
}

// stepRecordEdit [F33-22] 排在 stepMeal / stepWorkout 之前：「把午餐改成牛肉麵」符合記餐、「跑步改成 30 分鐘」符合記運動，
// 不先攔下來會被記成新的一筆. 沒命中 gate、功能沒啟用、規劃器回 None 時回 false，後面的步驟照舊.
func (h *WebhookHandler) stepRecordEdit(ctx context.Context, m *textMessage) bool {
	if !h.recordEditEnabled() {
		return false
	}
	// [F34-07] 助理剛提議「要不要改成…」，客人回「要」「好」這類肯定短句也送規劃器（閒聊的「好」照舊往下走）.
	gate := EditGate(m.text) || (isAffirmative(m.text) && h.assistant != nil && h.assistant.LastTurnOfferedEdit(ctx, m.userID))
	if !gate {
		return false
	}
	return h.runRecordEdit(ctx, m)
}

// affirmativeWords [F34-07] 肯定短句詞表（全等比對，去頭尾空白與句末「。！!～~」）；寫死在這一處，由 TestAffirmativeWordsTable 鎖住.
var affirmativeWords = map[string]bool{
	"要": true, "好": true, "好的": true, "好啊": true, "可以": true, "對": true, "嗯": true, "OK": true, "ok": true,
}

// isAffirmative [F34-07] 整句就是一個肯定短句（「要不要」「好像」「可以嗎」「對啊我午餐吃了牛肉麵」都不是）.
func isAffirmative(text string) bool {
	return affirmativeWords[strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), "。！!～~"))]
}

// stepRecordEditWorkoutRule [F33-22] 只攔「keyword 步驟會用規則記成新運動」的修改句（有運動名稱與數量、不是身體資料）：
// 先給修改規劃器一次機會；沒處理（回 false）就交給 keyword 照舊記運動.
func (h *WebhookHandler) stepRecordEditWorkoutRule(ctx context.Context, m *textMessage) bool {
	if !h.recordEditEnabled() || h.workoutRepo == nil || !EditGate(m.text) {
		return false
	}
	if _, isProfile := fitness.ParseProfile(m.text); isProfile {
		return false
	}
	if parsed := fitness.Parse(m.text); parsed.Activity == nil || !parsed.HasAmount() {
		return false
	}
	return h.runRecordEdit(ctx, m)
}

// hasEditableRecords [F33-22] 近 7 天有沒有可改的紀錄；沒有就不花 AI 次數（也不改變原本的行為）.
// 讀取失敗時當作沒有（訊息照舊往下走）.
func (h *WebhookHandler) hasEditableRecords(ctx context.Context, userID string) bool {
	todayStart, tomorrow := nutrition.DayRange(h.dailyNow())
	from := todayStart.AddDate(0, 0, -(editEditableDays - 1))
	if h.mealRepo != nil {
		ms, err := h.mealRepo.FindByUserBetween(ctx, userID, from, tomorrow)
		if err != nil {
			slog.Warn("record edit read meals", "error", redact.Error(err))
		} else if len(ms) > 0 {
			return true
		}
	}
	if h.workoutRepo != nil {
		ws, err := h.workoutRepo.FindByUserBetween(ctx, userID, from, tomorrow)
		if err != nil {
			slog.Warn("record edit read workouts", "error", redact.Error(err))
		} else if len(ws) > 0 {
			return true
		}
	}
	return false
}

// runRecordEdit [F33-22] 兩條路（關鍵字快判、意圖分類 edit_record）共用的入口；回傳 true = 已處理（已回覆），false = 交給後面的步驟.
// 順序：功能啟用 → 這則訊息還沒叫過（editTried）→ 有可改紀錄 → 時間預算 → 每日額度 → PlanEdit（1 次 AI）.
// 預算檢查排在額度之前：預算不足而跳過時不扣額度，客人沒享受到服務就不該少一次.
func (h *WebhookHandler) runRecordEdit(ctx context.Context, m *textMessage) bool {
	if !h.recordEditEnabled() || m.editTried {
		return false
	}
	// [F36-01] 額度用完：靜默跳過，且不標 editTried（沒叫過規劃器，問答不該帶不實的系統備註）.
	if !ai.CanSpend(ctx, false) {
		return false
	}
	m.editTried = true // 無論結果如何，同一則訊息不再叫第二次規劃器
	if !h.hasEditableRecords(ctx, m.userID) {
		return false
	}
	timeout := editTimeoutMax // ctx 沒有期限時視為剩餘時間無限大
	if deadline, ok := ctx.Deadline(); ok {
		var enough bool
		if timeout, enough = editTimeout(time.Until(deadline)); !enough {
			return false
		}
	}
	if !h.withinDailyLimit(ctx, "edit:count", m.userID, editDailyLimit) {
		_ = h.line.ReplyText(ctx, m.replyToken, fmt.Sprintf("今天的修改次數已達上限（%d 次），明天再來喔！", editDailyLimit))
		return true
	}
	if err := h.line.StartLoading(ctx, m.userID, loadingSeconds); err != nil {
		slog.Debug("start loading animation", "error", err)
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// [F34-07] 一次提議只換一次規劃器：三個入口（關鍵字、肯定短句、意圖分類）都經過這裡；
	// 預算、額度、沒有可改紀錄而提早返回時不會走到這行，所以不消耗.
	// ConsumeEditOffer 自己判 nil 與「最後一輪是不是提議」，不是提議時不寫 history.
	h.assistant.ConsumeEditOffer(ctx, m.userID, m.text)
	out, err := h.editPlanner.PlanEdit(pctx, m.userID, m.text)
	if err != nil || out == nil {
		if err != nil {
			slog.Warn("record edit plan failed", "error", redact.Error(err))
		}
		return false // 規劃器壞了：訊息照舊往下走，不卡住
	}
	switch out.Kind {
	case recordedit.OutcomeProposal:
		h.offerProposal(ctx, m, out)
		return true
	case recordedit.OutcomeClarify:
		h.offerCandidates(ctx, m, out)
		return true
	case recordedit.OutcomeInvalid:
		slog.Info("record edit invalid plan", "reason", reasonClass(out.Reason))
		_ = h.line.ReplyText(ctx, m.replyToken, invalidReply(out.Reason))
		return true
	default: // OutcomeNone：沒有要改，照舊交給後面（最後落到問答，維持原估算）
		return false
	}
}

// reasonClass Reason 的穩定前綴（log 只記類別；Reason 冒號後面是範圍提示，不是使用者文字，但仍只記前綴）.
func reasonClass(reason string) string {
	for _, p := range []string{
		recordedit.ReasonNoChange, recordedit.ReasonOutOfRange, recordedit.ReasonLastItem, recordedit.ReasonSameMeal, recordedit.ReasonBadPlan,
	} {
		if strings.HasPrefix(reason, p) {
			return p
		}
	}
	return "other"
}

// invalidReply [F33-23] OutcomeInvalid 依 Reason 前綴決定回哪句話；超範圍時把範圍提示原樣講給客人.
func invalidReply(reason string) string {
	switch reasonClass(reason) {
	case recordedit.ReasonNoChange:
		return editNoChangeMessage
	case recordedit.ReasonLastItem:
		return editLastItemMessage
	case recordedit.ReasonSameMeal: // [F34-12] 「這餐本來就是晚餐，不用改」；餐別名稱是程式產生的 Label
		return sameMealReply(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(reason, recordedit.ReasonSameMeal), ":")))
	case recordedit.ReasonOutOfRange:
		hint := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(reason, recordedit.ReasonOutOfRange), ":"))
		hint = recordedit.SanitizeDisplay(hint, 60)
		if hint == "" {
			return "這個數字看起來不太對，請確認後再說一次"
		}
		return "這個數字看起來不太對（" + hint + "），請確認後再說一次"
	default:
		return editInvalidMessage
	}
}

// storeKind log 用的暫存後端種類（redis|mongo；不是已知實作時為 other）.
func (h *WebhookHandler) storeKind() string {
	if b, ok := h.editStore.(interface{ Backend() string }); ok {
		return b.Backend()
	}
	return "other"
}

// putPending 存暫存並做寫後防護：Put 失敗 fail-closed 回「目前無法修改」；Put 之後客人刪了資料就補刪並回已刪除.
// 回傳 true = 已回覆（呼叫端不得再送確認卡）.
func (h *WebhookHandler) putPending(ctx context.Context, userID, replyToken string, p *recordedit.Pending) bool {
	if err := h.editStore.Put(ctx, userID, p); err != nil {
		slog.Error("record edit put pending failed", "store", h.storeKind(), "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return true
	}
	return h.guardAfterWrite(ctx, userID, replyToken, func(c context.Context) error { return h.editStore.Delete(c, userID) })
}

// newPending 產生帶 nonce 與到期時間的暫存項目.
func (h *WebhookHandler) newPending(kind recordedit.OutcomeKind) (*recordedit.Pending, error) {
	nonce, err := recordedit.NewNonce()
	if err != nil {
		return nil, err
	}
	return &recordedit.Pending{Nonce: nonce, Kind: kind, ExpiresAt: h.dailyNow().Add(recordedit.PendingTTL)}, nil
}

// confirmCard 確認卡：文字是 recordedit 程式樣板組出的 Summary（不含 AI 自由句），兩顆 postback 按鈕只帶 nonce.
func confirmCard(summary, nonce string) models.TextMessage {
	if utf8.RuneCountInString(summary) > maxEditSummaryRunes {
		summary = string([]rune(summary)[:maxEditSummaryRunes])
	}
	return models.NewText(summary,
		models.PostbackItem("✅ 確認", recordedit.PostbackData(recordedit.ActionOK, nonce, 0), "確認"),
		models.PostbackItem("❌ 取消", recordedit.PostbackData(recordedit.ActionCancel, nonce, 0), "取消"))
}

// offerProposal [F33-23] 提議不寫入：存暫存、出確認卡；按確認才寫.
func (h *WebhookHandler) offerProposal(ctx context.Context, m *textMessage, out *recordedit.Outcome) {
	if out.Proposal == nil || strings.TrimSpace(out.Proposal.Summary) == "" {
		_ = h.line.ReplyText(ctx, m.replyToken, editInvalidMessage)
		return
	}
	h.sendProposalCard(ctx, m.userID, m.replyToken, out.Proposal)
}

// sendProposalCard 把已驗證的提議存起來並回確認卡（一般提議與候選選定後共用）.
func (h *WebhookHandler) sendProposalCard(ctx context.Context, userID, replyToken string, prop *recordedit.Proposal) {
	p, err := h.newPending(recordedit.OutcomeProposal)
	if err != nil {
		slog.Error("record edit nonce failed", "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return
	}
	p.Proposal = prop
	if h.putPending(ctx, userID, replyToken, p) {
		return
	}
	slog.Info("record edit proposal offered", "kind", string(prop.Kind), "op", prop.Op, "store", h.storeKind())
	_ = h.line.Reply(ctx, replyToken, confirmCard(prop.Summary, p.Nonce))
}

// offerCandidates [F33-23] 多筆符合：存候選暫存、回候選按鈕（≤5）；label 在 webhook 端自己再截一次（不信任 Planner 的長度）.
func (h *WebhookHandler) offerCandidates(ctx context.Context, m *textMessage, out *recordedit.Outcome) {
	cands := make([]recordedit.Candidate, 0, maxEditCandidates)
	for _, c := range out.Candidates {
		label := truncateLabel(recordedit.SanitizeDisplay(c.Label, 0), maxQuickReplyLabelLen)
		if c.RecordID == "" || label == "" || len(cands) == maxEditCandidates {
			continue
		}
		cands = append(cands, recordedit.Candidate{RecordID: c.RecordID, Label: label})
	}
	if out.Plan == nil || len(cands) == 0 || (out.RecordKind != recordedit.KindMeal && out.RecordKind != recordedit.KindWorkout) {
		_ = h.line.ReplyText(ctx, m.replyToken, editInvalidMessage)
		return
	}
	p, err := h.newPending(recordedit.OutcomeClarify)
	if err != nil {
		slog.Error("record edit nonce failed", "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, m.replyToken, editUnavailableMessage)
		return
	}
	plan := *out.Plan
	p.Plan, p.RecordKind, p.Candidates = &plan, out.RecordKind, cands
	if h.putPending(ctx, m.userID, m.replyToken, p) {
		return
	}
	slog.Info("record edit candidates offered", "kind", string(out.RecordKind), "op", plan.Op, "count", len(cands), "store", h.storeKind())
	items := make([]models.QuickReplyItem, 0, len(cands)+1)
	for i, c := range cands {
		items = append(items, models.PostbackItem(c.Label, recordedit.PostbackData(recordedit.ActionPick, p.Nonce, i), c.Label))
	}
	items = append(items, models.PostbackItem("❌ 取消", recordedit.PostbackData(recordedit.ActionCancel, p.Nonce, 0), "取消"))
	_ = h.line.ReplyText(ctx, m.replyToken, editPickMessage, items...)
}
