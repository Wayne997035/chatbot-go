package webhook

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// 刪除個人資料後，刪除前就送出、還在處理中的訊息（例如分析到一半的照片）不能把資料寫回去.
const (
	// erasureTTL 處理中的工作最多約 1 分鐘；保留久一點以防萬一.
	erasureTTL       = 10 * time.Minute
	erasureKeyPrefix = "erased:"
	// erasureWarnAbove 本地標記筆數超過這個值才發 Warn（不拒收）.
	// 算式：每位刪除資料的使用者 1 筆，TTL 10 分鐘；假設 10 分鐘內最多 U=500 人刪除（估算值，未實測）= 500 筆，
	// 門檻 10_000 為其 20 倍，超過代表異常成長.
	erasureWarnAbove = 10000
	// erasureOpTimeout [F291-01][F291-02] 複查、補刪與標記的 Redis 讀寫各自的期限；不跟著請求 ctx 走.
	erasureOpTimeout = 5 * time.Second
)

// defaultErasureBudget [SEC29-03][F292-03] 事件 ctx 結束(關機取消或 55 秒期限)後,整則事件的複查與補刪最多還能做多久(所有段數共用,不是每段各一次).
// 必須小於 background.CancelGrace,關機才等得到它們做完. erasureBudget 是實際使用的預算,只有測試會暫時改短.
const defaultErasureBudget = erasureOpTimeout

var erasureBudget = defaultErasureBudget

type erasureBudgetKey struct{}

type erasureBudgetHolder struct{ ctx context.Context }

// withErasureBudget [SEC29-03][F292-03] 讓 ctx 底下的複查與補刪共用一個預算:ctx 結束後 erasureBudget 內全部收掉.
// 回傳的 done 在事件處理完時呼叫.
func withErasureBudget(ctx context.Context) (budgetCtx context.Context, done func()) {
	h := &erasureBudgetHolder{}
	budgetCtx = context.WithValue(ctx, erasureBudgetKey{}, h)
	b, cancel := context.WithCancel(context.WithoutCancel(budgetCtx))
	h.ctx = b
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(erasureBudget, cancel) })
	return budgetCtx, func() { stop(); cancel() }
}

// erasureOpCtx [SEC29-03][F292-03] 複查、補刪與標記讀寫用的 ctx:不跟著 ctx 取消,每段最多 erasureOpTimeout;
// ctx 帶了預算時(processEvents 設定),還受整則事件的預算限制.
func erasureOpCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if h, ok := ctx.Value(erasureBudgetKey{}).(*erasureBudgetHolder); ok && h.ctx != nil {
		return context.WithTimeout(h.ctx, erasureOpTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), erasureOpTimeout)
}

// erasures 記錄使用者最近一次刪除資料的時間. 有 Redis 時存 Redis，同時也記在記憶體（Redis 出錯時仍有效）.
type erasures struct {
	redis *redis.Client
	now   func() time.Time

	local *localcache.Cache[time.Time]
}

// newErasures [F24-33] 本地標記用不淘汰型 localcache：標記被丟 = 刪除後資料可能被寫回.
func newErasures(client *redis.Client) *erasures {
	e := &erasures{redis: client, now: time.Now}
	e.local = localcache.NewNoEvictWithClock[time.Time]("erasures", erasureTTL, erasureWarnAbove,
		func() time.Time { return e.now() })
	return e
}

// mark [F292-04] 記下現在刪除了這位使用者的資料. 本機一定記得；有 Redis 時也寫進 Redis，寫不進去回錯 ——
// 其他實例只看得到 Redis 的標記，呼叫端不能回覆已刪除.
func (e *erasures) mark(ctx context.Context, userID string) error {
	if e == nil {
		return nil
	}
	// [SEC29-02][F292-02] LINE 事件時間只到毫秒，標記也截到毫秒；同一毫秒判不出先後，算刪除之後（偏安全，決策 35b7cf2e）.
	at := e.now().Truncate(time.Millisecond)
	e.local.Set(userID, at)
	if e.redis == nil {
		return nil
	}
	// [F291-02] 請求 ctx 取消或到期時標記仍要寫進 Redis（go-redis 在 ctx 已取消時不送指令），否則多機的複查看不到標記.
	rctx, cancel := erasureOpCtx(ctx)
	defer cancel()
	if err := e.redis.Set(rctx, erasureKeyPrefix+userID, at.UnixMilli(), erasureTTL).Err(); err != nil {
		slog.Warn("record data erasure", "error", redact.Error(err))
		return err
	}
	return nil
}

// erasedSince [SEC29-02][F292-02] 刪除時間不早於 sentAt（同一毫秒算刪除過；這則訊息的結果不能再寫入）.
func (e *erasures) erasedSince(ctx context.Context, userID string, sentAt time.Time) bool {
	if e == nil {
		return false
	}
	at, ok := e.local.Get(userID)
	// [SEC29-01][F292-01] 本機有標記也要讀 Redis:多機時本機可能只有較舊的那次刪除.
	if e.redis != nil {
		// [F291-02] 讀標記同樣不受呼叫端 ctx 影響：ctx 已取消就讀不到 = 把「已刪除」誤判成「沒刪除」.
		rctx, cancel := erasureOpCtx(ctx)
		defer cancel()
		ms, err := e.redis.Get(rctx, erasureKeyPrefix+userID).Int64()
		switch {
		case err == nil:
			if rat := time.UnixMilli(ms); !ok || rat.After(at) {
				at, ok = rat, true
			}
		case !errors.Is(err, redis.Nil):
			slog.Warn("read data erasure mark", "error", redact.Error(err))
		}
	}
	return ok && !at.Before(sentAt) && e.now().Sub(at) <= erasureTTL
}

// ErasedSince [F241-12][F242-02] 刪除時間不早於 since（同一毫秒算刪除過；刪除標記 10 分鐘內有效）；警報推播以訂閱的更新時間當 since，
// 刪除後才重新訂閱的人不會被擋.
func (h *WebhookHandler) ErasedSince(ctx context.Context, userID string, since time.Time) bool {
	return h.erased.erasedSince(ctx, userID, since)
}

type sentAtKey struct{}

// withSentAt 把 LINE 事件的送出時間放進 ctx（毫秒；0 表示不知道）.
func withSentAt(ctx context.Context, timestampMS int64) context.Context {
	if timestampMS <= 0 {
		return ctx
	}
	return context.WithValue(ctx, sentAtKey{}, time.UnixMilli(timestampMS))
}

// sentAt 這則訊息的送出時間；不知道時用現在（也就是不會被擋）.
func sentAt(ctx context.Context) time.Time {
	if t, ok := ctx.Value(sentAtKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// erasedMessage 寫入前發現資料已被刪除時的回覆.
const erasedMessage = "你的資料已經刪除，這筆沒有儲存"

// eraseFailedMessage [F291-01] 補刪（或刪除）有一部分沒做成時的回覆；絕不能在這種情況回 erasedMessage.
const eraseFailedMessage = "部分資料刪除失敗，請稍後再試一次"

// erasedAfterSent [F291-01] 這則訊息送出後使用者刪除過資料（寫入前檢查的入口；erasure.go 以外不直接查標記）.
func (h *WebhookHandler) erasedAfterSent(ctx context.Context, userID string) bool {
	return h.erased.erasedSince(ctx, userID, sentAt(ctx))
}

// blockedByErasure 這則訊息送出後使用者刪除了資料，結果不能寫入；已回覆時回傳 true.
func (h *WebhookHandler) blockedByErasure(ctx context.Context, userID, replyToken string) bool {
	if !h.erasedAfterSent(ctx, userID) {
		return false
	}
	slog.Info("dropped write after data erasure", "sent_at_ms", strconv.FormatInt(sentAt(ctx).UnixMilli(), 10))
	_ = h.line.ReplyText(ctx, replyToken, erasedMessage)
	return true
}

// recheckAfterWrite [F291-01] 在「寫入之後」呼叫（寫入成功或回錯都要呼叫：回錯時資料可能已落地）.
// 刪除標記晚於這則訊息的送出時間 = 寫入落在刪除之後，呼叫 undo 補刪「這次寫的那筆」.
// 複查與補刪都不受請求 ctx 影響：請求到期或關機時補刪仍要做完. 讀標記由 erasedSince 自己限時；
// [R2-S4][F293-04] 命中之後才開補刪的 ctx（每段最多 erasureOpTimeout，ctx 帶預算時另受整則事件的預算限制），讀標記花掉的時間不從補刪扣掉.
// undo 回錯時記 Error（錯誤經 redact），並把錯誤回給呼叫端；呼叫端用 replyErasedAfterWrite 回實話.
func (h *WebhookHandler) recheckAfterWrite(
	ctx context.Context, userID string, undo func(ctx context.Context) error,
) (erased bool, undoErr error) {
	if !h.erased.erasedSince(ctx, userID, sentAt(ctx)) {
		return false, nil
	}
	cctx, cancel := erasureOpCtx(ctx)
	defer cancel()
	if err := undo(cctx); err != nil {
		slog.Error("undo write after data erasure", "error", redact.Error(err))
		return true, err
	}
	return true, nil
}

// replyErasedAfterWrite [F291-01] 補刪成功回 erasedMessage；補刪失敗回 eraseFailedMessage（絕不回 erasedMessage）.
func (h *WebhookHandler) replyErasedAfterWrite(ctx context.Context, replyToken string, undoErr error) {
	if undoErr != nil {
		_ = h.line.ReplyText(ctx, replyToken, eraseFailedMessage)
		return
	}
	_ = h.line.ReplyText(ctx, replyToken, erasedMessage)
}

// guardAfterWrite [F291-01] recheckAfterWrite 加 replyErasedAfterWrite：命中時已回覆並回傳 true，呼叫端不得再送成功回覆.
func (h *WebhookHandler) guardAfterWrite(ctx context.Context, userID, replyToken string, undo func(ctx context.Context) error) bool {
	erased, undoErr := h.recheckAfterWrite(ctx, userID, undo)
	if !erased {
		return false
	}
	h.replyErasedAfterWrite(ctx, replyToken, undoErr)
	return true
}

// undoState [F291-01] 補刪對話狀態（整份）；用會回傳錯誤的 Delete，不用吞錯的 clearState.
func (h *WebhookHandler) undoState(userID string) func(ctx context.Context) error {
	return func(ctx context.Context) error { return h.convManager.Delete(ctx, userID) }
}

// joinUndo [F291-01] 補刪多步驟：每一步都執行（前一步回錯不跳過後一步），任一步回錯就回錯.
func joinUndo(ctx context.Context, steps ...func(ctx context.Context) error) error {
	var errs []error
	for _, step := range steps {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
