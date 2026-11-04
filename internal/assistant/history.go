package assistant

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Turn 一輪問答.
type Turn struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
}

// History 最近幾輪對話，讓追問有上下文.
type History interface {
	Recent(ctx context.Context, userID string) []Turn
	Append(ctx context.Context, userID string, t Turn)
	// Clear 刪除使用者的對話紀錄；失敗要回報，刪除個人資料時才能告訴使用者重試.
	Clear(ctx context.Context, userID string) error
}

const (
	historyTurns  = 3
	historyTTL    = 30 * time.Minute
	historyPrefix = "qa:hist:"

	// [F24-46] 本地快取容量.
	historyLocalMaxSize = 5000
)

// RedisHistory 存在 Redis，30 分鐘沒聊就清掉；Redis 沒設定或出錯時改存本地快取.
type RedisHistory struct {
	client *redis.Client

	// [F24-46] localcache 沒有原子 read-modify-write，同一 user 的 Append 以 mu 保護.
	mu    sync.Mutex
	local *localcache.Cache[[]Turn]
}

// NewRedisHistory 建立對話紀錄；client 為 nil 時改存本地快取，永不回 nil.
func NewRedisHistory(client *redis.Client) History {
	return &RedisHistory{
		client: client,
		local:  localcache.New[[]Turn]("assistant.history", historyLocalMaxSize, historyTTL),
	}
}

// Recent 由舊到新.
func (h *RedisHistory) Recent(ctx context.Context, userID string) []Turn {
	if h.client != nil {
		vals, err := h.client.LRange(ctx, historyPrefix+userID, 0, historyTurns-1).Result()
		if err == nil {
			return decodeTurns(vals)
		}
		if errors.Is(err, redis.Nil) {
			return nil // 權威 miss，不查本地
		}
	}
	return h.recentLocal(userID)
}

// recentLocal 回傳本地快取的副本（由舊到新）.
func (h *RedisHistory) recentLocal(userID string) []Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	turns, ok := h.local.Get(userID)
	if !ok {
		return nil
	}
	return append([]Turn(nil), turns...)
}

func decodeTurns(vals []string) []Turn {
	turns := make([]Turn, 0, len(vals))
	for i := len(vals) - 1; i >= 0; i-- {
		var t Turn
		if json.Unmarshal([]byte(vals[i]), &t) == nil {
			turns = append(turns, t)
		}
	}
	return turns
}

// Append 新增一輪並只保留最近幾輪.
func (h *RedisHistory) Append(ctx context.Context, userID string, t Turn) {
	data, err := json.Marshal(t)
	if err != nil {
		return
	}
	if h.client != nil {
		key := historyPrefix + userID
		pipe := h.client.TxPipeline()
		pipe.LPush(ctx, key, data)
		pipe.LTrim(ctx, key, 0, historyTurns-1)
		pipe.Expire(ctx, key, historyTTL)
		if _, err := pipe.Exec(ctx); err == nil {
			return
		}
	}
	h.appendLocal(userID, t)
}

// appendLocal 讀出、附加、截最近幾輪、存回並重新計時（對齊 Redis 的 Expire）.
func (h *RedisHistory) appendLocal(userID string, t Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, _ := h.local.Get(userID)
	turns := append(append([]Turn(nil), old...), t)
	if len(turns) > historyTurns {
		turns = turns[len(turns)-historyTurns:]
	}
	h.local.SetTTL(userID, turns, historyTTL)
}

// Clear 清掉對話紀錄.
func (h *RedisHistory) Clear(ctx context.Context, userID string) error {
	h.mu.Lock()
	h.local.Delete(userID) // 一律先清本地，Redis 失敗時刪除資料流程才不殘留
	h.mu.Unlock()
	if h.client == nil {
		return nil
	}
	return h.client.Del(ctx, historyPrefix+userID).Err()
}

type noHistory struct{}

func (noHistory) Recent(context.Context, string) []Turn { return nil }
func (noHistory) Append(context.Context, string, Turn)  {}
func (noHistory) Clear(context.Context, string) error   { return nil }

func formatHistory(turns []Turn) string {
	if len(turns) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n【先前對話】\n")
	for _, t := range turns {
		sb.WriteString("使用者：" + t.Question + "\n助理：" + t.Answer + "\n")
	}
	return sb.String()
}

// Forget 清掉使用者的問答紀錄（刪除個人資料時使用）.
func (a *Assistant) Forget(ctx context.Context, userID string) error {
	if a == nil {
		return nil
	}
	return a.history.Clear(ctx, userID)
}
