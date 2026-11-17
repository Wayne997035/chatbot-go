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
		cur, err := h.mealRepo.FindByID(ctx, userID, id)
		if err != nil {
			slog.Error("record edit read candidate failed", "error", redact.Error(err))
			_ = h.line.ReplyText(ctx, replyToken, editFailedMessage)
			return
		}
		if cur == nil {
			_ = h.line.ReplyText(ctx, replyToken, editGoneMessage)
			return
		}
		prop, buildErr = recordedit.BuildMealProposal(&plan, cur)
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

// buildErrorReply Build*Proposal 的錯誤轉成給客人的話（與 invalidReply 同一套說法）.
func buildErrorReply(err error) string {
	switch {
	case errors.Is(err, recordedit.ErrNoChange):
		return editNoChangeMessage
	case errors.Is(err, recordedit.ErrLastItem):
		return editLastItemMessage
	case errors.Is(err, recordedit.ErrOutOfRange):
		return invalidReply(recordedit.ReasonOutOfRange + ": " + strings.TrimPrefix(err.Error(), recordedit.ErrOutOfRange.Error()+": "))
	}
	return editInvalidMessage
}
