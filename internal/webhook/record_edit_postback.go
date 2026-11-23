package webhook

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// [F33-19][F33-23] postback 事件：確認 / 取消 / 選候選.
// userID 只取 event.Source.UserID（handleEventSafely 在 switch 之前已檢查 Source.Type=="user"）；
// postback data 只帶一次性 nonce，不帶紀錄 ID 或數值，紀錄 ID 一律來自伺服端暫存.

// handlePostback [F33-19] 處理 postback 事件. 沒有啟用修改功能、data 不是本系統的格式 → 靜默忽略（不回覆、不 panic）.
// 不呼叫 saveUser（沒有新資料）、不動對話狀態.
func (h *WebhookHandler) handlePostback(ctx context.Context, event *models.Event) {
	if h.editStore == nil {
		return
	}
	pb, ok := recordedit.ParsePostback(event.Postback.Data)
	if !ok {
		slog.Debug("ignored postback with unrecognized data") // 不記 data 內容
		return
	}
	userID, replyToken := event.Source.UserID, event.ReplyToken
	// Take 原子「取出並刪除」= 單次使用；亂 nonce 回 ErrNotFound 且不動有效提議（儲存層保證）.
	p, err := h.editStore.Take(ctx, userID, pb.Nonce, h.dailyNow())
	if err != nil {
		if errors.Is(err, recordedit.ErrNotFound) {
			_ = h.line.ReplyText(ctx, replyToken, editExpiredMessage)
			return
		}
		slog.Error("record edit take pending failed", "store", h.storeKind(), "error", redact.Error(err))
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return
	}
	switch pb.Action {
	case recordedit.ActionCancel:
		slog.Info("record edit canceled")
		_ = h.line.ReplyText(ctx, replyToken, editCancelledMessage)
	case recordedit.ActionOK:
		if p.Kind != recordedit.OutcomeProposal || p.Proposal == nil {
			_ = h.line.ReplyText(ctx, replyToken, editExpiredMessage)
			return
		}
		h.confirmEdit(ctx, userID, replyToken, p.Proposal)
	case recordedit.ActionPick:
		h.pickCandidate(ctx, userID, replyToken, pb.Index, p)
	}
}

// confirmEdit [F33-23] 套用已確認的提議：寫入前檢查刪除資料、讀回最新紀錄、比對版本、寫入；回覆依實際寫入結果.
func (h *WebhookHandler) confirmEdit(ctx context.Context, userID, replyToken string, prop *recordedit.Proposal) {
	if h.blockedByErasure(ctx, userID, replyToken) {
		return
	}
	switch prop.Kind {
	case recordedit.KindMeal:
		h.confirmMeal(ctx, userID, replyToken, prop)
	case recordedit.KindWorkout:
		h.confirmWorkout(ctx, userID, replyToken, prop)
	default:
		_ = h.line.ReplyText(ctx, replyToken, editFailedMessage)
	}
}

// editFailed 寫入失敗的回覆（不含「已改」「已更新」「已刪除」）.
func (h *WebhookHandler) editFailed(ctx context.Context, replyToken string, prop *recordedit.Proposal, err error) {
	slog.Error("record edit write failed", "kind", string(prop.Kind), "op", prop.Op, "error", redact.Error(err))
	_ = h.line.ReplyText(ctx, replyToken, editFailedMessage)
}

func (h *WebhookHandler) confirmMeal(ctx context.Context, userID, replyToken string, prop *recordedit.Proposal) {
	if prop.Regroup != nil { // [F34-15] 改餐別 / 移品項 / 拆多餐不走單筆 ApplyMeal 流程
		h.confirmRegroup(ctx, userID, replyToken, prop)
		return
	}
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return
	}
	cur, err := h.mealRepo.FindByID(ctx, userID, prop.RecordID)
	if err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	if cur == nil {
		_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
		return
	}
	if cur.Version != prop.BaseVersion {
		_ = h.line.ReplyText(ctx, replyToken, editStaleMessage)
		return
	}
	if prop.Op == recordedit.OpDeleteMeal {
		ok, err := h.mealRepo.DeleteByID(ctx, userID, prop.RecordID)
		switch {
		case err != nil:
			h.editFailed(ctx, replyToken, prop, err)
		case !ok:
			_ = h.line.ReplyText(ctx, replyToken, "這筆已經刪除了")
		default:
			slog.Info("record edit applied", "kind", string(prop.Kind), "op", prop.Op)
			_ = h.line.ReplyText(ctx, replyToken, "已刪除 "+recordedit.MealLabel(cur)+"。"+h.todayTotalLine(ctx, userID))
		}
		return
	}
	if err := prop.ApplyMeal(cur); err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	ok, err := h.mealRepo.Update(ctx, cur)
	switch {
	case errors.Is(err, meal.ErrConflict):
		_ = h.line.ReplyText(ctx, replyToken, editStaleMessage)
	case err != nil:
		h.editFailed(ctx, replyToken, prop, err)
	case !ok:
		_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
	default:
		slog.Info("record edit applied", "kind", string(prop.Kind), "op", prop.Op)
		// 數字取自真的寫進去的紀錄（cur），不取提議上的數字.
		text := fmt.Sprintf("已改成 %.0f 大卡（原本 %.0f 大卡）。", cur.Total.Calories, prop.Before)
		if prop.Op == recordedit.OpRenameItem {
			text = fmt.Sprintf("已更新品項名稱（合計 %.0f 大卡）。", cur.Total.Calories)
		}
		_ = h.line.ReplyText(ctx, replyToken, text+h.todayTotalLine(ctx, userID))
	}
}

func (h *WebhookHandler) confirmWorkout(ctx context.Context, userID, replyToken string, prop *recordedit.Proposal) {
	if h.workoutRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return
	}
	cur, err := h.workoutRepo.FindByID(ctx, userID, prop.RecordID)
	if err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	if cur == nil {
		_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
		return
	}
	if cur.Version != prop.BaseVersion {
		_ = h.line.ReplyText(ctx, replyToken, editStaleMessage)
		return
	}
	if prop.Op == recordedit.OpDeleteWorkout {
		ok, err := h.workoutRepo.DeleteByID(ctx, userID, prop.RecordID)
		switch {
		case err != nil:
			h.editFailed(ctx, replyToken, prop, err)
		case !ok:
			_ = h.line.ReplyText(ctx, replyToken, "這筆已經刪除了")
		default:
			slog.Info("record edit applied", "kind", string(prop.Kind), "op", prop.Op)
			_ = h.line.ReplyText(ctx, replyToken, "已刪除 "+workoutLabel(cur)+"。")
		}
		return
	}
	if err := prop.ApplyWorkout(cur); err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	ok, err := h.workoutRepo.Update(ctx, cur)
	switch {
	case errors.Is(err, workout.ErrConflict):
		_ = h.line.ReplyText(ctx, replyToken, editStaleMessage)
	case err != nil:
		h.editFailed(ctx, replyToken, prop, err)
	case !ok:
		_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
	default:
		slog.Info("record edit applied", "kind", string(prop.Kind), "op", prop.Op)
		_ = h.line.ReplyText(ctx, replyToken, fmt.Sprintf("已更新 %s（%.0f 大卡）。", workoutLabel(cur), cur.Calories)+h.todayBurnedLine(ctx, userID))
	}
}

// workoutLabel 「11/16 19:00 跑步 30 分鐘・6 公里」；活動名稱經清理與截斷（資料庫裡的字串也不直接進回覆）.
func workoutLabel(w *workout.Workout) string {
	c := *w
	c.Activity = recordedit.SanitizeDisplay(w.Activity, recordedit.MaxNameRunes)
	return w.DoneAt.In(nutrition.Taipei).Format("01/02 15:04") + " " + fitness.Describe(&c)
}

// todayBurnedLine 「今日累計」運動消耗一行；讀取失敗時回傳空字串.
func (h *WebhookHandler) todayBurnedLine(ctx context.Context, userID string) string {
	ws, err := h.todayWorkouts(ctx, userID)
	if err != nil {
		slog.Warn("find today workouts", "error", redact.Error(err))
		return ""
	}
	total := 0.0
	for i := range ws {
		total += ws[i].Calories
	}
	return fmt.Sprintf("\n今日運動消耗：%.0f 大卡（%d 筆）", total, len(ws))
}

// pickCandidate [F33-23] 客人點了候選：用暫存裡的 RecordID（不從 data 取）讀回紀錄、驗歸屬（FindByID 帶 userID）、
// 產生提議、存新暫存（新 nonce）、出確認卡；點選後仍須再按確認才寫.
func (h *WebhookHandler) pickCandidate(ctx context.Context, userID, replyToken string, idx int, p *recordedit.Pending) {
	if p.Kind != recordedit.OutcomeClarify || p.Plan == nil || idx < 0 || idx >= len(p.Candidates) {
		// 暫存已被 Take 取走：不重新 Put，回過期（誠實告知，不是靜默）.
		_ = h.line.ReplyText(ctx, replyToken, editExpiredMessage)
		return
	}
	id, plan := p.Candidates[idx].RecordID, *p.Plan
	var prop *recordedit.Proposal
	var buildErr error
	switch p.RecordKind {
	case recordedit.KindMeal:
		if h.mealRepo == nil {
			_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
			return
		}
		var reply string
		if prop, reply, buildErr = h.buildPickedMeal(ctx, userID, id, &plan); reply != "" {
			_ = h.line.ReplyText(ctx, replyToken, reply)
			return
		}
	case recordedit.KindWorkout:
		if h.workoutRepo == nil {
			_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
			return
		}
		cur, err := h.workoutRepo.FindByID(ctx, userID, id)
		if err != nil {
			slog.Error("record edit read candidate failed", "error", redact.Error(err))
			_ = h.line.ReplyText(ctx, replyToken, editFailedMessage)
			return
		}
		if cur == nil {
			_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
			return
		}
		prop, buildErr = recordedit.BuildWorkoutProposal(&plan, cur, h.weightOf(ctx, userID), h.dailyNow())
	default:
		_ = h.line.ReplyText(ctx, replyToken, editExpiredMessage)
		return
	}
	if buildErr != nil {
		_ = h.line.ReplyText(ctx, replyToken, buildErrorReply(buildErr))
		return
	}
	h.sendProposalCard(ctx, userID, replyToken, prop)
}

// buildPickedMeal 客人點選的那筆餐點 → 提議. reply 非空 = 讀取失敗 / 已不在，呼叫端直接回這句；否則 prop 或 buildErr 二選一.
// [F34-12] regroup 計畫候選選定後也走同一份分組邏輯；併入目標要看來源那天的全部餐點，所以另外讀整天.
func (h *WebhookHandler) buildPickedMeal(ctx context.Context, userID, id string, plan *recordedit.Plan,
) (prop *recordedit.Proposal, reply string, buildErr error) {
	cur, err := h.mealRepo.FindByID(ctx, userID, id)
	if err != nil {
		slog.Error("record edit read candidate failed", "error", redact.Error(err))
		return nil, editFailedMessage, nil
	}
	if cur == nil {
		return nil, editGoneMessage, nil
	}
	if !recordedit.IsRegroupOp(plan.Op) {
		prop, buildErr = recordedit.BuildMealProposal(plan, cur)
		return prop, "", buildErr
	}
	from, to := nutrition.DayRange(cur.EatenAt)
	sameDay, err := h.mealRepo.FindByUserBetween(ctx, userID, from, to)
	if err != nil {
		slog.Error("record edit read same-day meals failed", "error", redact.Error(err))
		return nil, editFailedMessage, nil
	}
	prop, buildErr = recordedit.BuildRegroupProposal(plan, cur, sameDay, h.dailyNow())
	return prop, "", buildErr
}

// buildErrorReply Build*Proposal 的錯誤轉成給客人的話（與 invalidReply 同一套說法）.
func buildErrorReply(err error) string {
	switch {
	case errors.Is(err, recordedit.ErrNoChange):
		return editNoChangeMessage
	case errors.Is(err, recordedit.ErrLastItem):
		return editLastItemMessage
	case errors.Is(err, recordedit.ErrSameMealType):
		return sameMealReply(strings.TrimPrefix(err.Error(), recordedit.ErrSameMealType.Error()+": "))
	case errors.Is(err, recordedit.ErrOutOfRange):
		return invalidReply(recordedit.ReasonOutOfRange + ": " + strings.TrimPrefix(err.Error(), recordedit.ErrOutOfRange.Error()+": "))
	}
	return editInvalidMessage
}

// sameMealReply [F34-12] 整筆改餐別但本來就是那個餐別；label 是程式產生的餐別名稱（不是使用者文字），仍清理一次.
func sameMealReply(label string) string {
	label = recordedit.SanitizeDisplay(label, 10)
	if label == "" {
		return editNoChangeMessage
	}
	return "這餐本來就是" + label + "，不用改"
}

// regroupRun [F34-15] 一次改餐別 / 拆多餐的寫入進度：記下已 Insert 的 ID 與已 Update 的併入目標（快照與寫入後版本），失敗時逐步撤回.
type regroupRun struct {
	h         *WebhookHandler
	userID    string
	inserted  []string
	merged    []mergedMeal
	undoFails int
	// keep [F34-15] S 步回錯且無法確認是否落地：不得 undo（撤回會刪新餐並還原目標，來源若已瘦身或刪除就是遺失）.
	keep bool
}

// mergedMeal 一筆已寫入的併入目標：snap 是寫入前的快照，post 是寫入後的版本（撤回時版本相同才寫回）.
type mergedMeal struct {
	snap *meal.Meal
	post int64
}

// undo [F34-15] 逐步撤回已完成的寫入（用 erasureOpCtx，不吃請求 ctx 的到期）：
// 新開的紀錄 DeleteByID（只刪自己插的 ID）；併入目標在版本仍是自己寫入後的版本時才把快照寫回，被別人改過就不覆蓋；已不存在就略過.
// 撤回本身出錯或沒撤成的筆數累計在 undoFails（呼叫端回 editPartialMessage）.
func (r *regroupRun) undo(ctx context.Context) {
	uctx, cancel := erasureOpCtx(ctx)
	defer cancel()
	for _, id := range r.inserted {
		if _, err := r.h.mealRepo.DeleteByID(uctx, r.userID, id); err != nil {
			r.undoFails++
			slog.Error("record edit regroup undo insert failed", "error", redact.Error(err))
		}
	}
	for i := len(r.merged) - 1; i >= 0; i-- {
		mm := r.merged[i]
		cur, err := r.h.mealRepo.FindByID(uctx, r.userID, mm.snap.ID)
		switch {
		case err != nil:
			r.undoFails++
			slog.Error("record edit regroup undo read failed", "error", redact.Error(err))
			continue
		case cur == nil: // 期間被刪了（例如客人刪資料）：沒有東西可寫回
			continue
		case cur.Version != mm.post: // 期間又被別人改過：不覆蓋
			r.undoFails++
			slog.Error("record edit regroup undo skipped: record changed since our write")
			continue
		}
		back := recordedit.CloneMeal(mm.snap)
		back.Version = cur.Version
		// ok=false = 剛好被刪了，沒有東西可寫回，不算失敗.
		if _, err := r.h.mealRepo.Update(uctx, back); err != nil {
			r.undoFails++
			slog.Error("record edit regroup undo write failed", "error", redact.Error(err))
		}
	}
}

// confirmRegroup [F34-15] 套用已確認的 regroup 提議. 預檢 P1–P4 零寫入；寫入順序 W（Insert）→ M（併入目標 Update）→ S（來源最後）；
// 任何一步失敗都逐步撤回，撤回也失敗就回 editPartialMessage（絕不回成功句）；無 transaction，加法優先讓最壞情況是「品項同時在兩處」而不是遺失.
// 成功回覆的數字取自真的寫進去、剛回傳的紀錄.
func (h *WebhookHandler) confirmRegroup(ctx context.Context, userID, replyToken string, prop *recordedit.Proposal) {
	if h.mealRepo == nil {
		_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
		return
	}
	spec := prop.Regroup
	src, targets, reply, err := h.precheckRegroup(ctx, userID, spec)
	if err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	if reply != "" {
		_ = h.line.ReplyText(ctx, replyToken, reply)
		return
	}
	// P3：在最新讀出的紀錄上用同一份分組邏輯重算；P4：src 與 targets 沒被 ApplyRegroup 改動，直接當快照（讀出時已是複本）.
	writes, err := recordedit.ApplyRegroup(spec, src, targets)
	if err != nil {
		h.editFailed(ctx, replyToken, prop, err)
		return
	}
	run := &regroupRun{h: h, userID: userID}
	failReply, cause := run.write(ctx, spec, writes, targets)
	if cause != nil {
		slog.Error("record edit regroup write failed", "op", prop.Op, "error", redact.Error(cause),
			"inserted", len(run.inserted), "merged", len(run.merged))
	}
	if failReply != "" && !run.keep {
		run.undo(ctx)
	}
	// 寫入（或撤回）之後客人刪了資料：補刪新開的紀錄，刪除訊息優先於任何成功 / 失敗句.
	undoInserted := func(c context.Context) error {
		steps := make([]func(context.Context) error, 0, len(run.inserted))
		for _, id := range run.inserted {
			steps = append(steps, func(c context.Context) error {
				_, err := h.mealRepo.DeleteByID(c, userID, id)
				return err
			})
		}
		return joinUndo(c, steps...)
	}
	if h.guardAfterWrite(ctx, userID, replyToken, undoInserted) {
		return
	}
	switch {
	case failReply == "":
		slog.Info("record edit applied", "kind", string(prop.Kind), "op", prop.Op, "writes", len(writes))
		_ = h.line.ReplyText(ctx, replyToken, regroupSuccessText(src, writes)+h.todayTotalLine(ctx, userID))
	case run.keep:
		slog.Error("record edit regroup incomplete: source write outcome unknown, kept without undo", "op", prop.Op)
		_ = h.line.ReplyText(ctx, replyToken, editPartialMessage)
	case run.undoFails > 0:
		slog.Error("record edit regroup incomplete: undo did not fully succeed", "op", prop.Op, "undo_failed", run.undoFails)
		_ = h.line.ReplyText(ctx, replyToken, editPartialMessage)
	default:
		_ = h.line.ReplyText(ctx, replyToken, failReply)
	}
}

// precheckRegroup [F34-15] 預檢 P1、P2（零寫入）：來源與每個併入目標都用 userID 讀（跨使用者讀不到）；任一不在 → editGoneMessage；
// 任一版本與提議當下不同 → editStaleMessage（整張卡作廢，不寫任何一筆）. reply 非空 = 呼叫端直接回這句；err 非 nil = 讀取失敗.
func (h *WebhookHandler) precheckRegroup(ctx context.Context, userID string, spec *recordedit.RegroupSpec,
) (src *meal.Meal, targets map[string]*meal.Meal, reply string, err error) {
	src, err = h.mealRepo.FindByID(ctx, userID, spec.SourceID)
	if err != nil {
		return nil, nil, "", err
	}
	if src == nil {
		return nil, nil, editGoneMessage, nil
	}
	stale := src.Version != spec.SourceBase
	targets = map[string]*meal.Meal{}
	for i := range spec.Groups {
		g := &spec.Groups[i]
		if g.MergeID == "" {
			continue
		}
		t, err := h.mealRepo.FindByID(ctx, userID, g.MergeID)
		if err != nil {
			return nil, nil, "", err
		}
		if t == nil {
			return nil, nil, editGoneMessage, nil
		}
		targets[g.MergeID] = t
		stale = stale || t.Version != g.MergeBase
	}
	if stale {
		return nil, nil, editStaleMessage, nil
	}
	return src, targets, "", nil
}

// write [F34-15] 依序執行 W → M → S；任何一步失敗回要告訴客人的句子與原因（呼叫端負責撤回）；全部成功回 "", nil.
func (r *regroupRun) write(ctx context.Context, spec *recordedit.RegroupSpec, writes []recordedit.RegroupWrite,
	targets map[string]*meal.Meal,
) (string, error) {
	for i := range writes {
		w := &writes[i]
		ok, err := r.writeOne(ctx, spec, w, targets)
		// [F34-15] 非 ErrConflict 的錯誤：寫入可能已落地（逾時等），重讀判定；已落地就當成功往下走.
		if err != nil && !errors.Is(err, meal.ErrConflict) {
			landed, reply := r.settleWriteError(ctx, spec, w, targets, err)
			if reply != "" {
				return reply, err
			}
			if landed {
				continue
			}
		}
		if reply, err := writeFailure(ok, err); reply != "" {
			return reply, err
		}
	}
	return "", nil
}

// writeOne [F34-15] 執行一步寫入；Insert 與併入目標的進度記進 r（供撤回）.
func (r *regroupRun) writeOne(ctx context.Context, spec *recordedit.RegroupSpec, w *recordedit.RegroupWrite,
	targets map[string]*meal.Meal,
) (ok bool, err error) {
	repo := r.h.mealRepo
	ok = true
	switch w.Step {
	case recordedit.StepInsert:
		err = repo.Insert(ctx, w.Meal)
		if w.Meal.ID != "" { // Insert 回錯時資料可能已落地：ID 是剛產生的，記下來才撤得掉
			r.inserted = append(r.inserted, w.Meal.ID)
		}
	case recordedit.StepUpdateMerge:
		ok, err = repo.Update(ctx, w.Meal)
		if ok && err == nil {
			r.merged = append(r.merged, mergedMeal{snap: targets[w.Meal.ID], post: w.Meal.Version})
		}
	case recordedit.StepUpdateSource:
		ok, err = repo.Update(ctx, w.Meal)
	case recordedit.StepDeleteSource:
		ok, err = repo.DeleteIfVersion(ctx, r.userID, spec.SourceID, spec.SourceBase)
	}
	return ok, err
}

// writeFailure [F34-15] 一步寫入的結果對應要回的句子；成功回 "", nil.
func writeFailure(ok bool, err error) (string, error) {
	switch {
	case errors.Is(err, meal.ErrConflict):
		return editStaleMessage, err
	case err != nil:
		return editFailedMessage, err
	case !ok: // 讀出之後被刪了（含客人刪資料）
		return editGoneMessage, errors.New("record gone during regroup")
	}
	return "", nil
}

// writeLanded [F34-15] 重讀到的紀錄是不是我方這次寫入：版本 == 寫入前 + 1，且品項數與合計都等於要寫入的內容
// （版本 +1 但內容不同 = 同使用者的其他請求寫的，不能當成我方已落地）.
func writeLanded(got, want *meal.Meal, base int64) bool {
	return got.Version == base+1 && len(got.Items) == len(want.Items) && got.Total.Calories == want.Total.Calories
}

// settleWriteError [F34-15] write 的某一步回了非 ErrConflict 的錯誤時，用獨立期限（同 undo 的 erasureOpCtx）重讀該筆判定結果.
// 回 (true, "")：已落地，呼叫端繼續下一步；回 (false, "")：照舊當失敗（呼叫端回 editFailedMessage 並 undo）；
// 回 (_, 非空)：直接回這句（editGoneMessage，或 editPartialMessage 並設 r.keep 不 undo）.
func (r *regroupRun) settleWriteError(ctx context.Context, spec *recordedit.RegroupSpec, w *recordedit.RegroupWrite,
	targets map[string]*meal.Meal, writeErr error,
) (landed bool, reply string) {
	if w.Step == recordedit.StepInsert {
		return false, "" // Insert 另有 r.inserted 追蹤
	}
	id := spec.SourceID
	if w.Step == recordedit.StepUpdateMerge {
		id = w.Meal.ID
	}
	uctx, cancel := erasureOpCtx(ctx)
	defer cancel()
	cur, rerr := r.h.mealRepo.FindByID(uctx, r.userID, id)
	if rerr != nil {
		slog.Error("record edit regroup re-read failed", "step", int(w.Step), "error", redact.Error(rerr))
	}
	if w.Step == recordedit.StepUpdateMerge {
		return r.settleMerge(w, targets[id], cur, rerr, writeErr)
	}
	return r.settleSource(spec, w, cur, rerr, writeErr)
}

// settleMerge [F34-15] 併入目標：版本 == 快照 + 1 且內容相同 = 已落地（補記 r.merged）；不存在 = editGoneMessage；其餘照舊當失敗.
func (r *regroupRun) settleMerge(w *recordedit.RegroupWrite, snap, cur *meal.Meal, rerr, writeErr error) (landed bool, reply string) {
	switch {
	case rerr != nil || snap == nil:
		return false, ""
	case cur == nil:
		return false, editGoneMessage
	case writeLanded(cur, w.Meal, snap.Version):
		r.merged = append(r.merged, mergedMeal{snap: snap, post: cur.Version})
		slog.Error("record edit regroup merge write errored but landed", "step", int(w.Step), "error", redact.Error(writeErr))
		return true, ""
	}
	return false, ""
}

// settleSource [F34-15] 來源（UpdateSource / DeleteSource；S 是最後一步，不確定時不得撤回）：
// 確定沒寫入 → 照舊當失敗；已落地 → 成功；其餘（其他版本、不存在、重讀失敗）→ editPartialMessage 並設 r.keep.
func (r *regroupRun) settleSource(spec *recordedit.RegroupSpec, w *recordedit.RegroupWrite, cur *meal.Meal, rerr, writeErr error,
) (landed bool, reply string) {
	switch {
	case rerr == nil && cur != nil && cur.Version == spec.SourceBase:
		return false, "" // 確定沒寫入
	case rerr == nil && w.Step == recordedit.StepDeleteSource && cur == nil,
		rerr == nil && w.Step == recordedit.StepUpdateSource && cur != nil && writeLanded(cur, w.Meal, spec.SourceBase):
		slog.Error("record edit regroup source write errored but landed", "step", int(w.Step), "error", redact.Error(writeErr))
		return true, ""
	}
	r.keep = true
	slog.Error("record edit regroup source write outcome unknown, keeping writes", "step", int(w.Step), "error", redact.Error(writeErr))
	return false, editPartialMessage
}

// regroupSuccessText [F34-15] 成功回覆：每個寫進去的紀錄一行，數字取自寫入後回傳的紀錄（不取提議上的數字）.
func regroupSuccessText(src *meal.Meal, writes []recordedit.RegroupWrite) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "已調整 %s 的餐點：", src.EatenAt.In(nutrition.Taipei).Format("01/02"))
	// [F35-01] 每個名稱依該筆自己的 EatenAt：來源用來源的、寫入的用寫入後那筆的.
	srcLabel := nutrition.MealType(src.MealType).LabelAt(src.EatenAt)
	for i := range writes {
		w := &writes[i]
		switch {
		case w.Step == recordedit.StepInsert:
			fmt.Fprintf(&sb, "\n・%s %.0f 大卡（新增）", nutrition.MealType(w.Meal.MealType).LabelAt(w.Meal.EatenAt), w.Meal.Total.Calories)
		case w.Step == recordedit.StepUpdateMerge:
			fmt.Fprintf(&sb, "\n・%s %.0f 大卡", nutrition.MealType(w.Meal.MealType).LabelAt(w.Meal.EatenAt), w.Meal.Total.Calories)
		case w.Step == recordedit.StepUpdateSource && w.Meal.MealType != src.MealType: // 原地改餐別
			fmt.Fprintf(&sb, "\n・%s %.0f 大卡（原為%s）", nutrition.MealType(w.Meal.MealType).LabelAt(w.Meal.EatenAt), w.Meal.Total.Calories, srcLabel)
		case w.Step == recordedit.StepUpdateSource:
			fmt.Fprintf(&sb, "\n・%s %.0f 大卡（原 %.0f）", srcLabel, w.Meal.Total.Calories, src.Total.Calories)
		case w.Step == recordedit.StepDeleteSource:
			fmt.Fprintf(&sb, "\n・%s 這筆已刪除（原 %.0f）", srcLabel, src.Total.Calories)
		}
	}
	return sb.String()
}

// stepRecordEditConfirmText [F34-17] 打字「確認」「取消」＝按確認卡的按鈕. 只認 trim 後全等「確認」「取消」（「確認刪除」「好」「ok」都不處理）；
// 功能停用時不處理（維持現狀）. userID 只取 webhook 事件發送者（m.userID），打字從不帶 nonce，無法指定別人的卡或舊卡；
// PeekPending 不消耗，確認 / 取消再用既有的原子 Take，所以按鈕、打字、雙擊競爭同一張卡時只有一方成功.
func (h *WebhookHandler) stepRecordEditConfirmText(ctx context.Context, m *textMessage) bool {
	if !h.recordEditEnabled() {
		return false
	}
	word := strings.TrimSpace(m.text)
	if word != typedConfirm && word != typedCancel {
		return false
	}
	now := h.dailyNow()
	p, err := h.editStore.PeekPending(ctx, m.userID, now)
	if err != nil {
		h.replyStoreErr(ctx, m.replyToken, "peek", err, editNoPendingMessage)
		return true
	}
	if word == typedConfirm && p.Kind != recordedit.OutcomeProposal {
		_ = h.line.ReplyText(ctx, m.replyToken, editPickFirstMessage) // 候選清單還沒選：不 Take、暫存保留
		return true
	}
	taken, err := h.editStore.Take(ctx, m.userID, p.Nonce, now)
	// 找不到代表卡已被按鈕或另一則打字搶先處理.
	if err != nil {
		h.replyStoreErr(ctx, m.replyToken, "take", err, editExpiredMessage)
		return true
	}
	if word == typedCancel {
		slog.Info("record edit canceled")
		_ = h.line.ReplyText(ctx, m.replyToken, editCancelledMessage)
		return true
	}
	if taken.Kind != recordedit.OutcomeProposal || taken.Proposal == nil {
		_ = h.line.ReplyText(ctx, m.replyToken, editExpiredMessage)
		return true
	}
	h.confirmEdit(ctx, m.userID, m.replyToken, taken.Proposal) // 與按鈕同一條
	return true
}

// replyStoreErr 暫存後端的錯誤回覆：ErrNotFound 回 notFound 那句；其他錯誤記 Error（經 redact）並回「目前無法修改」.
func (h *WebhookHandler) replyStoreErr(ctx context.Context, replyToken, op string, err error, notFound string) {
	if errors.Is(err, recordedit.ErrNotFound) {
		_ = h.line.ReplyText(ctx, replyToken, notFound)
		return
	}
	slog.Error("record edit pending "+op+" failed", "store", h.storeKind(), "error", redact.Error(err))
	_ = h.line.ReplyText(ctx, replyToken, editUnavailableMessage)
}

// 打字確認 / 取消的全等字串（與確認卡按鈕的 displayText 相同）.
const (
	typedConfirm = "確認"
	typedCancel  = "取消"
)
