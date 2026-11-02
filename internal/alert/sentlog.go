package alert

import (
	"context"
	"log/slog"
	"sync"
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
	// maxLocalSent 記憶體版超過這個數量才清理過期紀錄.
	maxLocalSent = 1000
)

// sentLog 記錄「這則警報已經送給這個人」. 有 Redis 時存 Redis；Redis 沒設定或出錯時改用程式內記憶體，
// 寧可重啟後重送一次，也不要每 5 分鐘對所有人重送（每月推播額度只有 200 則）.
type sentLog struct {
	redis *redis.Client
	now   func() time.Time

	mu    sync.Mutex
	local map[string]time.Time // key → 到期時間
	tries map[string]int
}

func newSentLog(client *redis.Client) *sentLog {
	return &sentLog{redis: client, now: time.Now, local: map[string]time.Time{}, tries: map[string]int{}}
}

// claim 送出前先佔位；回傳 false 表示已經送過（或正在送）.
func (s *sentLog) claim(ctx context.Context, key string) bool {
	key = sentKeyPrefix + key
	if s.redis != nil {
		ok, err := s.redis.SetNX(ctx, key, "1", sentTTL).Result()
		if err == nil {
			return ok
		}
		slog.Warn("alert sent-log unavailable, using memory", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.local) >= maxLocalSent {
		s.pruneLocked()
	}
	if exp, ok := s.local[key]; ok && s.now().Before(exp) {
		return false
	}
	s.local[key] = s.now().Add(sentTTL)
	return true
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
			}
			return
		}
		slog.Warn("alert sent-log unavailable, using memory", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries[key]++
	if s.tries[key] < maxPushTries {
		delete(s.local, key)
	}
}

// pruneLocked 清掉記憶體版過期的紀錄；呼叫端需持有 mu.
func (s *sentLog) pruneLocked() {
	now := s.now()
	for k, exp := range s.local {
		if now.After(exp) {
			delete(s.local, k)
			delete(s.tries, k)
		}
	}
}
