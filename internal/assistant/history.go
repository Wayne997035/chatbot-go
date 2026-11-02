package assistant

import (
	"context"
	"encoding/json"
	"strings"
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
)

// RedisHistory 存在 Redis，30 分鐘沒聊就清掉.
type RedisHistory struct {
	client *redis.Client
}

// NewRedisHistory 建立對話紀錄；client 為 nil 時回傳 nil（不記上下文）.
func NewRedisHistory(client *redis.Client) History {
	if client == nil {
		return nil
	}
	return &RedisHistory{client: client}
}

// Recent 由舊到新.
func (h *RedisHistory) Recent(ctx context.Context, userID string) []Turn {
	vals, err := h.client.LRange(ctx, historyPrefix+userID, 0, historyTurns-1).Result()
	if err != nil {
		return nil
	}
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
	key := historyPrefix + userID
	pipe := h.client.TxPipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, historyTurns-1)
	pipe.Expire(ctx, key, historyTTL)
	_, _ = pipe.Exec(ctx)
}

// Clear 清掉對話紀錄.
func (h *RedisHistory) Clear(ctx context.Context, userID string) error {
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
