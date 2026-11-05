// Package recentmeal 記住每位使用者「剛記錄的那一餐」，讓後續的份量按鈕、補照片、店家、取消等操作對應到它.
package recentmeal

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// TTL 份量、店家、取消、再加等操作可以作用在剛記的那餐多久.
	TTL = 30 * time.Minute
	// KeyPrefix Redis key 前綴（後面接 userID）.
	KeyPrefix = "meal:last:"
	// [F24-47] 本地快取容量上限；滿載時新 key 可能被拒收（localcache 會 Warn）.
	localCapacity = 5000
)

// Meal 剛記錄的一餐；只存 ID 與來源，內容以資料庫為準.
type Meal struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// Store 有 Redis 時存 Redis（多台共用）；Redis 沒設定或出錯時存本地快取. nil 的 Store 什麼都不記.
type Store struct {
	redis *redis.Client
	// Now 目前時間；測試可替換.
	Now func() time.Time

	local *localcache.Cache[Meal]
	// clock 本地快取背景維護 goroutine 讀的時間；只由呼叫端 goroutine 以 syncClock 更新，
	// 避免背景 goroutine 直接讀可被測試替換的 Now 欄位而產生 data race.
	clock atomic.Int64
}

// New 建立；client 為 nil 或出錯時存本地快取.
func New(client *redis.Client) *Store {
	s := &Store{redis: client, Now: time.Now}
	s.syncClock()
	// 本地快取的時鐘跟著 Store.Now（每次操作前由 syncClock 同步）.
	s.local = localcache.NewWithClock[Meal]("recentmeal", localCapacity, TTL, func() time.Time { return time.Unix(0, s.clock.Load()) })
	return s
}

// syncClock 把 Store.Now 的目前值同步給本地快取的時鐘.
func (s *Store) syncClock() { s.clock.Store(s.Now().UnixNano()) }

// Remember 記住使用者剛記錄的那一餐.
func (s *Store) Remember(ctx context.Context, userID, id, source string) {
	if s == nil {
		return
	}
	s.syncClock()
	m := Meal{ID: id, Source: source, At: s.Now()}
	if s.redis != nil {
		data, err := json.Marshal(m)
		if err != nil {
			return
		}
		err = s.redis.Set(ctx, KeyPrefix+userID, data, TTL).Err()
		if err == nil {
			// [F24-71] Redis 已寫入最新值，清掉故障期間留在本地的舊值，避免 Redis 再故障時讀回過期的那一餐.
			s.local.Delete(userID)
			return
		}
		slog.Warn("recent meal Redis write failed, falling back to local cache", "error", redact.Error(err))
	}
	s.local.Set(userID, m)
}

// Get 回傳 TTL 內剛記錄的那一餐.
func (s *Store) Get(ctx context.Context, userID string) (Meal, bool) {
	if s == nil {
		return Meal{}, false
	}
	s.syncClock()
	var m Meal
	fromLocal := s.redis == nil
	if s.redis != nil {
		data, err := s.redis.Get(ctx, KeyPrefix+userID).Bytes()
		switch {
		case err == nil:
			if json.Unmarshal(data, &m) != nil {
				return Meal{}, false
			}
		case errors.Is(err, redis.Nil):
			return Meal{}, false // 權威 miss，不查本地
		default:
			slog.Warn("recent meal Redis read failed, falling back to local cache", "error", redact.Error(err))
			fromLocal = true
		}
	}
	if fromLocal {
		m, _ = s.local.Get(userID)
	}
	if m.ID == "" || s.Now().Sub(m.At) > TTL {
		return Meal{}, false
	}
	return m, true
}

// Forget 刪除使用者的紀錄；Redis 失敗時回傳錯誤（刪除個人資料時要能告訴使用者重試）.
func (s *Store) Forget(ctx context.Context, userID string) error {
	if s == nil {
		return nil
	}
	s.syncClock()
	s.local.Delete(userID) // 一律先清本地，Redis 失敗時刪除資料流程才不殘留
	if s.redis == nil {
		return nil
	}
	return s.redis.Del(ctx, KeyPrefix+userID).Err()
}
