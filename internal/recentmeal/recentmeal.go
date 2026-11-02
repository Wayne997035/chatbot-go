// Package recentmeal 記住每位使用者「剛記錄的那一餐」，讓後續的份量按鈕、補照片、店家、取消等操作對應到它.
package recentmeal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// TTL 份量、店家、取消、再加等操作可以作用在剛記的那餐多久.
	TTL = 30 * time.Minute
	// KeyPrefix Redis key 前綴（後面接 userID）.
	KeyPrefix = "meal:last:"
	// maxLocal 沒有 Redis 時記憶體版超過這個數量就清掉過期的.
	maxLocal = 1000
)

// Meal 剛記錄的一餐；只存 ID 與來源，內容以資料庫為準.
type Meal struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// Store 有 Redis 時存 Redis（多台共用），否則存記憶體（測試、單機）. nil 的 Store 什麼都不記.
type Store struct {
	redis *redis.Client
	// Now 目前時間；測試可替換.
	Now func() time.Time

	mu    sync.Mutex
	local map[string]Meal
}

// New 建立；client 為 nil 時存記憶體.
func New(client *redis.Client) *Store {
	return &Store{redis: client, Now: time.Now, local: map[string]Meal{}}
}

// Remember 記住使用者剛記錄的那一餐.
func (s *Store) Remember(ctx context.Context, userID, id, source string) {
	if s == nil {
		return
	}
	m := Meal{ID: id, Source: source, At: s.Now()}
	if s.redis == nil {
		s.mu.Lock()
		if len(s.local) >= maxLocal {
			s.pruneLocked()
		}
		s.local[userID] = m
		s.mu.Unlock()
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := s.redis.Set(ctx, KeyPrefix+userID, data, TTL).Err(); err != nil {
		slog.Warn("remember recent meal", "error", err)
	}
}

// Get 回傳 TTL 內剛記錄的那一餐.
func (s *Store) Get(ctx context.Context, userID string) (Meal, bool) {
	if s == nil {
		return Meal{}, false
	}
	var m Meal
	if s.redis == nil {
		s.mu.Lock()
		m = s.local[userID]
		s.mu.Unlock()
	} else {
		data, err := s.redis.Get(ctx, KeyPrefix+userID).Bytes()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				slog.Warn("get recent meal", "error", err)
			}
			return Meal{}, false
		}
		if json.Unmarshal(data, &m) != nil {
			return Meal{}, false
		}
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
	if s.redis == nil {
		s.mu.Lock()
		delete(s.local, userID)
		s.mu.Unlock()
		return nil
	}
	return s.redis.Del(ctx, KeyPrefix+userID).Err()
}

// pruneLocked 清掉記憶體版裡過期的紀錄；呼叫端需持有 mu.
func (s *Store) pruneLocked() {
	for id, m := range s.local {
		if s.Now().Sub(m.At) > TTL {
			delete(s.local, id)
		}
	}
}
