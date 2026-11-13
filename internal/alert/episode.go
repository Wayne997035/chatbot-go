package alert

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

const (
	// episodeTTL 天氣特報「進行中」標記：每次檢查看到就續期；連續這麼久沒看到才算解除，之後再發布是新的一則.
	// 要大於最長的抓取間隔，持續中的特報才不會被當成解除. 低溫明細的間隔取自設定（CWASettings().ColdInterval，預設 30 分鐘）；
	// [F30-43] 低溫那條路徑的標記改用 Checker.coldEpisodeTTL（隨間隔拉長），其他特報仍是這個值.
	episodeTTL       = 2 * time.Hour
	episodeKeyPrefix = "alert:episode:"
	// episodeWarnAbove 本地標記筆數超過只發 Warn：同時進行中的「縣市 + 等級」最多約 22 縣市 × 數種特報，遠低於此.
	episodeWarnAbove = 1000
)

// episodes 天氣特報進行中標記. 值是這一波第一次看到的時間，放進去重 id：
// 持續期間 id 不變（每人只推一次），解除超過 episodeTTL 後再發布，id 換新、會再推.
// 有 Redis 時存 Redis；Redis 沒設定或出錯時改用程式內記憶體（寧可重啟後重推一次，也不漏推）.
type episodes struct {
	redis *redis.Client
	local *localcache.Cache[string]
}

func newEpisodes(client *redis.Client) *episodes {
	return &episodes{redis: client, local: localcache.NewNoEvict[string]("alert-episode", episodeTTL, episodeWarnAbove)}
}

// newEpisodesWithClock 測試用：注入時鐘，2 小時到期不必真的等.
func newEpisodesWithClock(client *redis.Client, now func() time.Time) *episodes {
	return &episodes{redis: client, local: localcache.NewNoEvictWithClock[string]("alert-episode", episodeTTL, episodeWarnAbove, now)}
}

// start 回傳 key 這一波的開始時間（沒有進行中的標記就以 now 開始新的一波），並把標記續期 episodeTTL.
func (e *episodes) start(ctx context.Context, key string, now time.Time) string {
	return e.startTTL(ctx, key, now, episodeTTL)
}

// startTTL [F30-43] 同 start，但標記存活時間由呼叫端決定（低溫明細的抓取間隔可由設定拉長）.
func (e *episodes) startTTL(ctx context.Context, key string, now time.Time, ttl time.Duration) string {
	key = episodeKeyPrefix + key
	fresh := strconv.FormatInt(now.Unix(), 10)
	if e.redis != nil {
		v, err := e.redisStart(ctx, key, fresh, ttl)
		if err == nil {
			return v
		}
		slog.Warn("alert episode store unavailable, keeping it in memory", "error", redact.Error(err))
	}
	if v, ok := e.local.Get(key); ok {
		e.local.SetTTL(key, v, ttl)
		return v
	}
	if e.local.SetIfAbsent(key, fresh, ttl) {
		return fresh
	}
	// 並行的檢查剛寫入：用它的值.
	if v, ok := e.local.Get(key); ok {
		return v
	}
	return fresh
}

// redisStart 平常 2 個指令（GET + EXPIRE）；沒有標記時 SET NX，並行搶輸就再讀一次.
func (e *episodes) redisStart(ctx context.Context, key, fresh string, ttl time.Duration) (string, error) {
	v, err := e.redis.Get(ctx, key).Result()
	switch {
	case err == nil:
		if err := e.redis.Expire(ctx, key, ttl).Err(); err != nil {
			return "", err
		}
		return v, nil
	case !errors.Is(err, redis.Nil):
		return "", err
	}
	ok, err := e.redis.SetNX(ctx, key, fresh, ttl).Result()
	if err != nil {
		return "", err
	}
	if ok {
		return fresh, nil
	}
	return e.redis.Get(ctx, key).Result()
}
