package alert

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// scanPageSize SCAN 每次往返的建議筆數；只影響往返次數，換頁會一直跑到游標回 0.
const scanPageSize = 100

// globEscaper 把 Redis MATCH glob 的特殊字元跳脫，userID 才不會被當成樣式.
var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)

// [F241-11] forgetRecipient 刪除這位收件人的所有推播紀錄（Redis 佔位與 :tries、本地佔位與計數）.
// 先清本地：Redis 為 nil 或出錯時，本地備援的紀錄也不能留著. 任一 Redis SCAN / DEL 失敗就停下回傳錯誤，錯誤不含 userID 與鍵.
func (s *sentLog) forgetRecipient(ctx context.Context, userID string) error {
	if userID == "" {
		// 空 userID 會讓樣式變成 alert:sent:*: ，刪到別人的鍵.
		return nil
	}
	suffix := ":" + userID
	s.sent.DeleteMatching(func(k string) bool {
		return strings.HasPrefix(k, sentKeyPrefix) && strings.HasSuffix(k, suffix)
	})
	s.tries.DeleteMatching(func(k string) bool {
		return strings.HasPrefix(k, sentKeyPrefix) && strings.HasSuffix(k, suffix)
	})
	if s.redis == nil {
		return nil
	}
	esc := globEscaper.Replace(userID)
	for _, pattern := range []string{sentKeyPrefix + "*:" + esc, sentKeyPrefix + "*:" + esc + ":tries"} {
		if err := s.deleteByPattern(ctx, pattern); err != nil {
			return fmt.Errorf("forget alert history: %w", err)
		}
	}
	return nil
}

// [F241-11] deleteByPattern SCAN 到游標回 0 為止，逐鍵 DEL（不依賴 cluster 拓撲）.
func (s *sentLog) deleteByPattern(ctx context.Context, pattern string) error {
	var cursor uint64
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, pattern, scanPageSize).Result()
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := s.redis.Del(ctx, k).Err(); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// [F241-11] forgetKey 刪掉一把完整鍵（未加前綴的 <種類>:<id>:<userID>）的佔位與重試計數.
// Redis 刪不掉只 Warn（鍵 7 天到期），Warn 不含鍵；本地一律刪.
func (s *sentLog) forgetKey(ctx context.Context, key string) {
	key = sentKeyPrefix + key
	if s.redis != nil {
		if err := s.redis.Del(ctx, key).Err(); err != nil {
			slog.Warn("forget alert sent key failed", "error", redact.Error(err))
		}
		if err := s.redis.Del(ctx, key+":tries").Err(); err != nil {
			slog.Warn("forget alert tries key failed", "error", redact.Error(err))
		}
	}
	s.sent.Delete(key)
	s.tries.DeleteMatching(func(k string) bool { return k == key })
}
