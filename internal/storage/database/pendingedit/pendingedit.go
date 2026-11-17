// Package pendingedit [F33-21] 聊天修改紀錄的「待確認提議」暫存：Redis（有連上時）或 MongoDB（沒有 Redis 時）.
// 兩種實作與記憶體實作（storagetest.PendingEdits）由同一組契約測試（storagetest.PendingStoreContract）規範：
// 每個 userID 最多一筆、Take 原子「比對 nonce、檢查未過期、取出並刪除」、亂 nonce 不消耗有效提議.
package pendingedit

import (
	"chatbot-go/internal/assistant/recordedit"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// 後端種類（Backend() 的回傳值；log 與測試用，不含使用者資料）.
const (
	BackendRedis = "redis"
	BackendMongo = "mongo"
)

// errInvalidPut Put 的輸入不合法（空 userID、nil、nonce 格式錯）.
var errInvalidPut = errors.New("pendingedit: invalid put")

// validPut 輸入檢查：nonce 一定是 recordedit.NewNonce 的格式，後端比對才有意義.
func validPut(userID string, p *recordedit.Pending) error {
	if userID == "" || p == nil || !recordedit.ValidNonce(p.Nonce) {
		return errInvalidPut
	}
	return nil
}

func encode(p *recordedit.Pending) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode pending edit: %w", err)
	}
	return string(b), nil
}

func decode(data string) (*recordedit.Pending, error) {
	var p recordedit.Pending
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return nil, fmt.Errorf("decode pending edit: %w", err)
	}
	return &p, nil
}

// Select [F33-21] 啟動時依 Redis 有沒有連上選後端：有 → Redis，沒有（app.go 連不上已把 client 置 nil）→ mongoStore.
// 只在啟動時選一次；執行期單次呼叫失敗一律回 error（呼叫端 fail-closed），不動態切到另一個後端（一筆提議散在兩邊會破壞單次使用）.
func Select(redisClient *redis.Client, mongoStore recordedit.Store) recordedit.Store {
	if redisClient != nil {
		return NewRedisStore(redisClient)
	}
	return mongoStore
}

// BackendOf 回傳 store 的後端種類；不是本套件的實作回空字串.
func BackendOf(s recordedit.Store) string {
	switch v := s.(type) {
	case *RedisStore:
		return v.Backend()
	case *MongoStore:
		return v.Backend()
	}
	return ""
}
