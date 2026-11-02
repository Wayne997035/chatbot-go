package webhook

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 刪除個人資料後，刪除前就送出、還在處理中的訊息（例如分析到一半的照片）不能把資料寫回去.
const (
	// erasureTTL 處理中的工作最多約 1 分鐘；保留久一點以防萬一.
	erasureTTL       = 10 * time.Minute
	erasureKeyPrefix = "erased:"
)

// erasures 記錄使用者最近一次刪除資料的時間. 有 Redis 時存 Redis，同時也記在記憶體（Redis 出錯時仍有效）.
type erasures struct {
	redis *redis.Client
	now   func() time.Time

	mu    sync.Mutex
	local map[string]time.Time
}

func newErasures(client *redis.Client) *erasures {
	return &erasures{redis: client, now: time.Now, local: map[string]time.Time{}}
}

// mark 記下現在刪除了這位使用者的資料.
func (e *erasures) mark(ctx context.Context, userID string) {
	if e == nil {
		return
	}
	// LINE 事件時間只到毫秒；用同樣的精度比較，刪除同一毫秒內送出的訊息不會被誤擋.
	at := e.now().Truncate(time.Millisecond)
	e.mu.Lock()
	for id, t := range e.local {
		if at.Sub(t) > erasureTTL {
			delete(e.local, id)
		}
	}
	e.local[userID] = at
	e.mu.Unlock()
	if e.redis != nil {
		if err := e.redis.Set(ctx, erasureKeyPrefix+userID, at.UnixMilli(), erasureTTL).Err(); err != nil {
			slog.Warn("record data erasure", "error", err)
		}
	}
}

// erasedSince 使用者在 sentAt 之後刪除過資料（這則訊息的結果不能再寫入）.
func (e *erasures) erasedSince(ctx context.Context, userID string, sentAt time.Time) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	at, ok := e.local[userID]
	e.mu.Unlock()
	if !ok && e.redis != nil {
		if ms, err := e.redis.Get(ctx, erasureKeyPrefix+userID).Int64(); err == nil {
			at, ok = time.UnixMilli(ms), true
		}
	}
	return ok && at.After(sentAt) && e.now().Sub(at) <= erasureTTL
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

// blockedByErasure 這則訊息送出後使用者刪除了資料，結果不能寫入；已回覆時回傳 true.
func (h *WebhookHandler) blockedByErasure(ctx context.Context, userID, replyToken string) bool {
	if !h.erased.erasedSince(ctx, userID, sentAt(ctx)) {
		return false
	}
	slog.Info("dropped write after data erasure", "sent_at_ms", strconv.FormatInt(sentAt(ctx).UnixMilli(), 10))
	_ = ReplyText(ctx, replyToken, erasedMessage)
	return true
}
