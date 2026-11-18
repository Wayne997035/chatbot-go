package pendingedit

import (
	"chatbot-go/internal/assistant/recordedit"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisKeyPrefix = "pedit:"
	// redisTTL [F33-21] Redis key 的存活時間 = recordedit.PendingTTL；Take 另外還會以傳入的 now 比對 ExpiresAt.
	redisTTL = recordedit.PendingTTL
)

// takeScript [F33-21] 在 Redis 伺服器端原子執行：nonce 相等才讀出資料並刪除 key；不相等什麼都不動（亂 nonce 不能毀掉有效提議）.
// 回 false（nil reply）= 沒有這個 key 或 nonce 不符.
// 用法：redis.NewScript + Run（EVALSHA，沒有腳本時退回 EVAL）— https://redis.uptrace.dev/guide/lua-scripting.html.
var takeScript = redis.NewScript(`
local n = redis.call('HGET', KEYS[1], 'nonce')
if (not n) or n ~= ARGV[1] then
  return false
end
local d = redis.call('HGET', KEYS[1], 'data')
redis.call('DEL', KEYS[1])
return d
`)

// RedisStore recordedit.Store 的 Redis 實作：key pedit:<userID>，hash 欄位 nonce、data（Pending JSON），EXPIRE 300 秒.
type RedisStore struct {
	c *redis.Client
}

var _ recordedit.Store = (*RedisStore)(nil)

// NewRedisStore 建立 Redis 版暫存.
func NewRedisStore(c *redis.Client) *RedisStore { return &RedisStore{c: c} }

// Backend 後端種類.
func (s *RedisStore) Backend() string { return BackendRedis }

func redisKey(userID string) string { return redisKeyPrefix + userID }

// Put 覆蓋該使用者既有的項目（DEL + HSET + EXPIRE 在同一個 MULTI/EXEC 內，不會留下半套資料）.
func (s *RedisStore) Put(ctx context.Context, userID string, p *recordedit.Pending) error {
	if err := validPut(userID, p); err != nil {
		return err
	}
	data, err := encode(p)
	if err != nil {
		return err
	}
	key := redisKey(userID)
	if _, err := s.c.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, key)
		pipe.HSet(ctx, key, "nonce", p.Nonce, "data", data)
		pipe.Expire(ctx, key, redisTTL)
		return nil
	}); err != nil {
		return fmt.Errorf("put pending edit: %w", err)
	}
	return nil
}

// Take 原子取出並刪除；nonce 不符、不存在、已過期一律 recordedit.ErrNotFound.
// nonce 比對在 Redis 的 Lua 內（與刪除同一個原子步驟，Go 端先比再刪會有雙擊競態）；
// key 以事件 userID 為名、nonce 128 bit 隨機，沒有可利用的時間側通道.
func (s *RedisStore) Take(ctx context.Context, userID, nonce string, now time.Time) (*recordedit.Pending, error) {
	if userID == "" || !recordedit.ValidNonce(nonce) {
		return nil, recordedit.ErrNotFound
	}
	res, err := takeScript.Run(ctx, s.c, []string{redisKey(userID)}, nonce).Text()
	if errors.Is(err, redis.Nil) {
		return nil, recordedit.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("take pending edit: %w", err)
	}
	p, err := decode(res)
	if err != nil {
		return nil, err
	}
	if !now.Before(p.ExpiresAt) {
		return nil, recordedit.ErrNotFound
	}
	return p, nil
}

// PeekPending [F34-16] 只讀不刪（HGET，不經 takeScript）；過期判斷與 Take 相同（!now.Before(ExpiresAt)）.
func (s *RedisStore) PeekPending(ctx context.Context, userID string, now time.Time) (*recordedit.Pending, error) {
	if userID == "" {
		return nil, recordedit.ErrNotFound
	}
	res, err := s.c.HGet(ctx, redisKey(userID), "data").Result()
	if errors.Is(err, redis.Nil) {
		return nil, recordedit.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("peek pending edit: %w", err)
	}
	p, err := decode(res)
	if err != nil {
		return nil, err
	}
	if !now.Before(p.ExpiresAt) {
		return nil, recordedit.ErrNotFound
	}
	return p, nil
}

// Delete 刪除該使用者的項目；不存在不算錯.
func (s *RedisStore) Delete(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	if err := s.c.Del(ctx, redisKey(userID)).Err(); err != nil {
		return fmt.Errorf("delete pending edit: %w", err)
	}
	return nil
}
