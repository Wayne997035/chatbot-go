package alert

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// 逐位收件人的推播紀錄.
const (
	// sentTTL 同一則警報對同一個人多久內不重送；地震、海嘯另外有 maxAlertAge 過濾舊資料.
	sentTTL = 7 * 24 * time.Hour
	// maxPushTries 推播失敗最多重試幾次（每輪檢查一次），之後放棄，避免對永久失敗的人一直重送.
	maxPushTries  = 3
	sentKeyPrefix = "alert:sent:"
	// sentLogWarnAbove 本地紀錄筆數超過這個數字只發 Warn，不淘汰、不拒寫（淘汰 = 重推，吃每月 200 則額度）.
	// 算式：訂閱者 S=500 × 7 天內每人不同警報 A=20 = 10_000（S、A 為估算值，未實測）；
	// 每筆約 200 B，10_000 筆約 2 MB. tries 的 key 數 ≤ 佔位筆數，沿用同一門檻.
	sentLogWarnAbove = 10_000
)

// sentLog 記錄「這則警報已經送給這個人」. 有 Redis 時存 Redis；Redis 沒設定或出錯時改用程式內記憶體，
// 寧可重啟後重送一次，也不要每 5 分鐘對所有人重送（每月推播額度只有 200 則）.
type sentLog struct {
	redis *redis.Client
	// [F24-51] sent 本地佔位：NoEvict，不因數量淘汰，只靠 TTL 或 Delete 移除.
	sent *localcache.Cache[struct{}]
	// [F24-52] tries 本地重試次數：Counter，同樣不淘汰.
	tries *localcache.Counter
}

func newSentLog(client *redis.Client) *sentLog {
	return &sentLog{
		redis: client,
		sent:  localcache.NewNoEvict[struct{}]("alert-sent", sentTTL, sentLogWarnAbove),
		tries: localcache.NewCounter("alert-tries", sentLogWarnAbove),
	}
}

// newSentLogWithClock 測試用：注入時鐘，7 天到期不必真的等.
func newSentLogWithClock(client *redis.Client, now func() time.Time) *sentLog {
	return &sentLog{
		redis: client,
		sent:  localcache.NewNoEvictWithClock[struct{}]("alert-sent", sentTTL, sentLogWarnAbove, now),
		tries: localcache.NewCounterWithClock("alert-tries", sentLogWarnAbove, now),
	}
}

// claim 送出前先佔位；回傳 false 表示已經送過（或正在送）.
func (s *sentLog) claim(ctx context.Context, key string) bool {
	key = sentKeyPrefix + key
	if s.redis != nil {
		ok, err := s.redis.SetNX(ctx, key, "1", sentTTL).Result()
		if err == nil {
			return ok
		}
		slog.Warn("alert sent-log unavailable, claim recorded in memory", "error", redact.Error(err))
	}
	// [F24-51] 原子 insert-if-absent，回傳值即 claim 結果.
	return s.sent.SetIfAbsent(key, struct{}{}, sentTTL)
}

// failed 推播失敗：還沒到重試上限就釋放佔位，下一輪只重試這個人.
func (s *sentLog) failed(ctx context.Context, key string) {
	key = sentKeyPrefix + key
	if s.redis != nil {
		tries, err := s.redis.Incr(ctx, key+":tries").Result()
		if err == nil {
			s.redis.Expire(ctx, key+":tries", sentTTL)
			if tries < maxPushTries {
				s.redis.Del(ctx, key)
				// [F24-71] 同時釋放故障期間留在本地的佔位，否則 Redis 再故障時 claim 讀本地回 false，該重試的警報不再重試.
				s.sent.Delete(key)
			}
			return
		}
		slog.Warn("alert sent-log unavailable, failed attempt counted in memory", "error", redact.Error(err))
	}
	// [F24-52] 本地計數；達上限保留佔位（永不再推）.
	n := s.tries.Incr(key, sentTTL)
	if n < maxPushTries {
		s.sent.Delete(key)
	}
}
